package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"golang.org/x/net/context"
	"golang.org/x/oauth2/google"
	sd "google.golang.org/api/logging/v2beta1"

	kubeapi "k8s.io/api/core/v1"
)

var (
	// Example of an image pulling message:
	//   pulling image "gcr.io/google_containers/echoserver:1.6"
	imagePullingMsgRegex = regexp.MustCompile(`pulling image "([a-z0-9.\-:_/]+)"`)
	// Container image "gcr.io/google-containers/nginx-slim-amd64:0.20" already
	// present on machine
	imagePulledMsgRegex1 = regexp.MustCompile(`Container image "([a-z0-9.\-:_/]+)"`)
	// Successfully pulled image
	// "gcr.io/google_containers/cluster-proportional-autoscaler-amd64:1.1.2-r2"
	imagePulledMsgRegex2 = regexp.MustCompile(`Successfully pulled image "([a-z0-9.\-:_/]+)"`)
)

func newSDService() (*sd.Service, error) {
	// projectID := framework.TestContext.CloudConfig.ProjectID
	ctx := context.Background()
	hc, err := google.DefaultClient(ctx, sd.CloudPlatformReadOnlyScope)
	sdService, err := sd.New(hc)
	return sdService, err
}

func buildFilter(projectID, clusterName, reason string) string {
	conditions := []string{
		"resource.type=\"gke_cluster\"",
		fmt.Sprintf("resource.labels.project_id =\"%s\"", projectID),
		fmt.Sprintf("resource.labels.cluster_name=\"%s\"", clusterName),
		fmt.Sprintf("jsonPayload.reason=\"%s\"", reason),
	}
	return strings.Join(conditions, " AND ")
}

func extractImageNameFromPulledEvent(event *kubeapi.Event) (string, error) {
	for _, re := range []*regexp.Regexp{imagePulledMsgRegex1, imagePulledMsgRegex2} {
		matches := re.FindStringSubmatch(event.Message)
		if len(matches) == 2 {
			return matches[1], nil
		} else if len(matches) > 2 {
			return "", fmt.Errorf("found more than one match when extracting the image name: %+v", matches)
		}
	}
	return "", fmt.Errorf("could not extract image name from %q\n", event.Message)
}

func main() {
	// resource.type="gke_cluster" AND
	// resource.labels.project_id ="gke-up-c1-4-glat-up-clu-n" AND
	// resource.labels.cluster_name="e2e-17523" AND
	// jsonPayload.reason="Pulling"
	svc, err := newSDService()
	if err != nil {
		fmt.Printf("failed to create a new StackDriver service: %v", err)
		os.Exit(1)
	}

	filter := buildFilter("gke-up-c1-4-glat-up-clu-n", "e2e-17523", "Pulled")
	req := &sd.ListLogEntriesRequest{
		ResourceNames: []string{"projects/gke-up-c1-4-glat-up-clu-n"},
		Filter:        filter,
	}

	// fmt.Printf("Filter: %s\n", filter)
	imageRegistry := map[string]struct{}{}

	pageToken := ""
	for {
		req.PageToken = pageToken
		res, err := svc.Entries.List(req).Do()
		if err != nil {
			fmt.Printf("failed to get the log entries: %v", err)
			os.Exit(1)
		}

		// Examine the response
		// fmt.Printf("\nNextPageToken: %v\n", res.NextPageToken)
		pageToken = res.NextPageToken
		// fmt.Printf("Entries (len: %d):\n", len(res.Entries))
		for _, entry := range res.Entries {
			var event kubeapi.Event
			if err := json.Unmarshal(entry.JsonPayload, &event); err != nil {
				fmt.Printf("Failed to unmarshal into an Event): %v", err)
				os.Exit(1)
			}
			image, err := extractImageNameFromPulledEvent(&event)
			if err != nil {
				fmt.Println(err)
				continue
			}
			imageRegistry[image] = struct{}{}
		}

		// If pageToken is empty, we're done.
		if pageToken == "" {
			break
		}
	}

	// Print all images in the registry.
	// TODO: Group the images by namespace.
	for k, _ := range imageRegistry {
		fmt.Printf("%q\n", k)
	}
}
