/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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
	"archive/tar"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/engine-api/client"
	"github.com/docker/engine-api/types"
	"github.com/docker/engine-api/types/container"
	"github.com/docker/engine-api/types/network"
	"github.com/docker/engine-api/types/strslice"
	"github.com/golang/glog"
	"github.com/pborman/uuid"
	"golang.org/x/net/context"
)

func init() {
	flag.Set("logtostderr", "true")
}

func getDockerClient(dockerEndpoint string) (*client.Client, error) {
	glog.Infof("Connecting to docker on %s\n", dockerEndpoint)
	return client.NewClient(dockerEndpoint, "", nil, nil)
}

func parseImage(imageStr string) (string, string, error) {
	if len(imageStr) == 0 {
		return "", "", fmt.Errorf("image name must be non-empty")
	}
	chunks := strings.Split(imageStr, ":")
	switch len(chunks) {
	case 1:
		return chunks[0], "", nil
	case 2:
		return chunks[0], chunks[1], nil
	default:
		return "", "", fmt.Errorf("invalid image name %q; expect <image:tag>", imageStr)
	}
}

func pullImage(client *client.Client, image string) error {
	imageID, tag, err := parseImage(image)
	if err != nil {
		return err
	}
	resp, err := client.ImagePull(
		context.Background(),
		types.ImagePullOptions{
			ImageID: imageID,
			Tag:     tag,
		},
		nil,
	)
	if err != nil {
		return err
	}
	defer resp.Close()
	decoder := json.NewDecoder(resp)
	for {
		var msg jsonmessage.JSONMessage
		err := decoder.Decode(&msg)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if msg.Error != nil {
			return msg.Error
		}
	}
	return nil
}

func createContainer(client *client.Client, containerName, imageName string) (string, error) {
	hc := &container.HostConfig{}
	nc := &network.NetworkingConfig{}
	cc := &container.Config{
		Image: imageName,
		// A bogus command to make sure docker doesn't complain.
		Entrypoint: strslice.StrSlice([]string{"ls"}),
	}
	resp, err := client.ContainerCreate(context.Background(), cc, hc, nc, containerName)
	if err != nil {
		return "", fmt.Errorf("failed to create container %q: %v", containerName, err)
	}
	// Ignore warnings for now.
	return resp.ID, nil
}

func untar(reader io.Reader, dest string) error {
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		} else if err != nil {
			return err
		}

		path := filepath.Join(dest, header.Name)
		info := header.FileInfo()
		if info.IsDir() {
			if err = os.MkdirAll(path, info.Mode()); err != nil {
				return err
			}
			continue
		}

		file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(file, tarReader)
		if err != nil {
			return err
		}
	}
	return nil
}

func removeContainer(client *client.Client, containerID string) error {
	return client.ContainerRemove(context.Background(), types.ContainerRemoveOptions{ContainerID: containerID, Force: true})
}

func exportContainer(client *client.Client, containerID, path string) error {
	in, err := client.ContainerExport(context.Background(), containerID)
	if err != nil {
		return fmt.Errorf("failed to export container %q: %v", containerID, err)
	}
	defer in.Close()
	if err := untar(in, path); err != nil {
		return fmt.Errorf("failed to untar the content of container %q: %v", containerID, err)
	}

	return nil
}

func ensureRootfsDir(path string) error {
	fInfo, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Create a new directory if it does not exist.
			return os.Mkdir(path, 0755)
		} else {
			return fmt.Errorf("unable to stat the given directory %q: %v", path, err)
		}
	} else if !fInfo.IsDir() {
		return fmt.Errorf("path %q is not a directory", path)
	}
	return nil
}

func main() {
	go func() {
		ticker := time.Tick(time.Second)
		for {
			<-ticker
			glog.Flush()
		}
	}()

	imagePtr := flag.String("image", "", "Image to fetch")
	rootfsPtr := flag.String("rootfs-dir", "/tmp/rootfs", "Path to store the rootfs")
	flag.Parse()

	c, err := getDockerClient("unix:///var/run/docker.sock")
	if err != nil {
		glog.Fatalf("Unable to connect to docker: %v", err)
	}

	glog.Infof("Starting to pull image %q", *imagePtr)
	if err := pullImage(c, *imagePtr); err != nil {
		glog.Fatalf("Failed to pull image %q: %v", *imagePtr, err)
	}
	glog.Infof("Successfully pulled image %q", *imagePtr)

	// Serialize the image.
	err = ensureRootfsDir(*rootfsPtr)
	if err != nil {
		glog.Fatalf("Invalid rootfs directory %q: %v", *rootfsPtr, err)
	}

	// Step 1: Create a temporary container so that we can export the
	// filesystem.

	// Use a random string as the container name to avoid conflict.
	cName := uuid.NewUUID().String()
	cID, err := createContainer(c, cName, *imagePtr)
	if err != nil {
		glog.Fatalf("Unable to create a temporary container for image %q: %v", *imagePtr, err)
	}
	glog.Infof("Succesfully created a temporary container %q", cID)
	defer func() {
		// Remove the temporary container.
		if err := removeContainer(c, cID); err != nil {
			glog.Warningf("Unable to remove the temporary container %q: %v", cID, err)
		} else {
			glog.Infof("Successfully removed container %q", cID)
		}
	}()

	// Step 2: Export the filesystem of the container to the given directory..
	if err := exportContainer(c, cID, *rootfsPtr); err != nil {
		glog.Fatalf("Unable to export the temporary container %q: %v", cID, err)
	}
	glog.Infof("Succesfully exported container to %q", *rootfsPtr)
}
