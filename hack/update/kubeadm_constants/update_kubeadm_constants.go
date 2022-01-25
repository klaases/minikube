/*
Copyright 2021 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"

	"golang.org/x/mod/semver"
	"k8s.io/klog/v2"
	"k8s.io/minikube/hack/update"
	"k8s.io/minikube/pkg/minikube/constants"
)

const (
	// default context timeout
	cxTimeout                 = 300 * time.Second
	kubeadmReleaseURL         = "https://storage.googleapis.com/kubernetes-release/release/%s/bin/linux/amd64/kubeadm"
	kubeadmBinaryName         = "kubeadm-linux-amd64-%s"
	minikubeConstantsFilePath = "pkg/minikube/constants/constants_kubeadm_images.go"
	kubeadmImagesTemplate     = `
		{{- range $version, $element := .}}
		"{{$version}}": {
			{{- range $image, $tag := $element}}
			"{{$image}}": "{{$tag}}",
			{{- end}}
		},{{- end}}`
)

// Data contains kubeadm Images map
type Data struct {
	ImageMap string `json:"ImageMap"`
}

func main() {

	inputVersion := flag.Lookup("kubernetes-version").Value.String()

	imageVersions := make([]string, 0)

	// set a context with defined timeout
	ctx, cancel := context.WithTimeout(context.Background(), cxTimeout)
	defer cancel()
	if inputVersion == "latest" {
		stableImageVersion, latestImageVersion, edgeImageVersion, err := getK8sVersions(ctx, "kubernetes", "kubernetes")
		if err != nil {
			klog.Fatal(err)
		}
		imageVersions = append(imageVersions, stableImageVersion, latestImageVersion, edgeImageVersion)
	} else if semver.IsValid(inputVersion) {
		imageVersions = append(imageVersions, inputVersion)
	} else {
		klog.Fatal(errors.New("invalid version"))
	}

	for _, imageVersion := range imageVersions {
		imageMapString, err := getKubeadmImagesMapString(imageVersion)
		if err != nil {
			klog.Fatalln(err)
		}

		var data Data
		schema := map[string]update.Item{
			minikubeConstantsFilePath: {
				Replace: map[string]string{},
			},
		}

		majorMinorVersion := semver.MajorMinor(imageVersion)

		if _, ok := constants.KubeadmImages[majorMinorVersion]; !ok {
			data = Data{ImageMap: imageMapString}
			schema[minikubeConstantsFilePath].Replace[`KubeadmImages = .*`] =
				`KubeadmImages = map[string]map[string]string{ {{.ImageMap}}`
		} else {
			data = Data{ImageMap: strings.TrimLeft(imageMapString, "\n")}
			versionIdentifier := fmt.Sprintf(`"%s": {[^}]+},`, majorMinorVersion)
			schema[minikubeConstantsFilePath].Replace[versionIdentifier] = "{{.ImageMap}}"
		}

		update.Apply(ctx, schema, data, "", "", -1)
	}
}

func getKubeadmImagesMapString(version string) (string, error) {
	url := fmt.Sprintf(kubeadmReleaseURL, version)
	fileName := fmt.Sprintf(kubeadmBinaryName, version)
	if err := downloadFile(url, fileName); err != nil {
		klog.Errorf("failed to download kubeadm binary %s", err.Error())
		return "", err
	}

	kubeadmCommand := fmt.Sprintf("./%s", fileName)
	args := []string{"config", "images", "list"}
	imageListString, err := executeCommand(kubeadmCommand, args...)
	if err != nil {
		klog.Errorf("failed to execute kubeadm command %s", kubeadmCommand)
		return "", err
	}

	if err := os.Remove(fileName); err != nil {
		klog.Errorf("failed to remove binary %s", fileName)
	}

	return formatKubeadmImageList(version, imageListString)
}

func formatKubeadmImageList(version, data string) (string, error) {
	templateData := make(map[string]map[string]string)
	majorMinorVersion := semver.MajorMinor(version)
	templateData[majorMinorVersion] = make(map[string]string)
	lines := strings.Split(data, "\n")
	for _, line := range lines {
		imageTag := strings.Split(line, ":")
		if len(imageTag) != 2 {
			continue
		}
		// removing the repo from image name
		imageName := strings.Split(imageTag[0], "/")
		imageTag[0] = strings.Join(imageName[1:], "/")
		if !isKubeImage(imageTag[0]) {
			templateData[majorMinorVersion][imageTag[0]] = imageTag[1]
		}
	}

	imageTemplate := template.New("kubeadmImage")
	t, err := imageTemplate.Parse(kubeadmImagesTemplate)
	if err != nil {
		klog.Errorf("failed to create kubeadm image map template %s", err.Error())
		return "", err
	}

	var bytesBuffer bytes.Buffer
	if err := t.Execute(&bytesBuffer, &templateData); err != nil {
		return "", err
	}

	return bytesBuffer.String(), nil
}

func isKubeImage(name string) bool {
	kubeImages := map[string]bool{
		"kube-apiserver":          true,
		"kube-controller-manager": true,
		"kube-proxy":              true,
		"kube-scheduler":          true,
	}
	return kubeImages[name]
}

func downloadFile(url, fileName string) error {
	file, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer file.Close()

	response, err := http.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("non success status code, while downloading file: %s from: %s", fileName, url)
	}

	if _, err := io.Copy(file, response.Body); err != nil {
		return err
	}

	return os.Chmod(fileName, os.ModePerm)
}

func executeCommand(command string, args ...string) (string, error) {
	output, err := exec.Command(command, args...).Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// getK8sVersion returns Kubernetes versions.
func getK8sVersions(ctx context.Context, owner, repo string) (stable, latest, edge string, err error) {
	// get Kubernetes versions from GitHub Releases
	stable, latest, edge, err = update.GHReleases(ctx, owner, repo)
	if err != nil {
		return "", "", "", err
	}

	if !semver.IsValid(stable) || !semver.IsValid(latest) || !semver.IsValid(edge) {
		return "", "", "", fmt.Errorf("invalid release obtained stable : %s, latest : %s, edge: %s", stable, latest, edge)
	}

	return stable, latest, edge, nil
}
