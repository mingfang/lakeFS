package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/Azure/azure-pipeline-go/pipeline"
	"github.com/Azure/azure-storage-blob-go/azblob"
	"github.com/go-openapi/swag"
)

var (
	ErrAzureInvalidURL  = errors.New("invalid Azure storage URL")
	ErrAzureCredentials = errors.New("azure credentials error")
)

func getAzureClient() (pipeline.Pipeline, error) {
	// From the Azure portal, get your storage account name and key and set environment variables.
	accountName, accountKey := os.Getenv("AZURE_STORAGE_ACCOUNT"), os.Getenv("AZURE_STORAGE_ACCESS_KEY")
	if len(accountName) == 0 || len(accountKey) == 0 {
		return nil, fmt.Errorf("%w: either the AZURE_STORAGE_ACCOUNT or AZURE_STORAGE_ACCESS_KEY environment variable is not set", ErrAzureCredentials)
	}

	// Create a default request pipeline using your storage account name and account key.
	credential, err := azblob.NewSharedKeyCredential(accountName, accountKey)
	if err != nil {
		return nil, fmt.Errorf("invalid credentials with error: %w", err)
	}
	return azblob.NewPipeline(credential, azblob.PipelineOptions{}), nil
}

func NewAzureBlobWalker(svc pipeline.Pipeline) (*azureBlobWalker, error) {
	return &azureBlobWalker{
		client: svc,
		mark:   Mark{HasMore: true},
	}, nil
}

type azureBlobWalker struct {
	client pipeline.Pipeline
	mark   Mark
}

// extractAzurePrefix takes a URL that looks like this: https://storageaccount.blob.core.windows.net/container/prefix
// and return the URL for the container and a prefix, if one exists
func extractAzurePrefix(storageURI *url.URL) (*url.URL, string, error) {
	path := strings.TrimLeft(storageURI.Path, "/")
	if len(path) == 0 {
		return nil, "", fmt.Errorf("%w: could not parse container URL: %s", ErrAzureInvalidURL, storageURI)
	}
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 1 {
		// we only have a container
		return storageURI, "", nil
	}
	// we have both prefix and storage container, rebuild URL
	relativePath := url.URL{Path: "/" + parts[0]}
	return storageURI.ResolveReference(&relativePath), parts[1], nil
}

func getAzureBlobURL(containerURL *url.URL, blobName string) *url.URL {
	relativePath := url.URL{Path: containerURL.Path + "/" + blobName}
	return containerURL.ResolveReference(&relativePath)
}

func (a *azureBlobWalker) Walk(ctx context.Context, storageURI *url.URL, op WalkOptions, walkFn func(e ObjectStoreEntry) error) error {
	// we use bucket as container and prefix as path
	containerURL, prefix, err := extractAzurePrefix(storageURI)
	if err != nil {
		return err
	}
	container := azblob.NewContainerURL(*containerURL, a.client)
	notDone := true
	for marker := (azblob.Marker{Val: &op.ContinuationToken}); notDone; {
		listBlob, err := container.ListBlobsFlatSegment(ctx, marker,
			azblob.ListBlobsSegmentOptions{Prefix: prefix})
		if err != nil {
			return err
		}
		a.mark.ContinuationToken = swag.StringValue(marker.Val)
		marker = listBlob.NextMarker
		for _, blobInfo := range listBlob.Segment.BlobItems {
			// skipping everything in the page which is before 'After' (without forgetting the possible empty string key!)
			if op.After != "" && blobInfo.Name <= op.After {
				continue
			}
			a.mark.LastKey = blobInfo.Name
			if err := walkFn(ObjectStoreEntry{
				FullKey:     blobInfo.Name,
				RelativeKey: strings.TrimPrefix(blobInfo.Name, prefix),
				Address:     getAzureBlobURL(containerURL, blobInfo.Name).String(),
				ETag:        string(blobInfo.Properties.Etag),
				Mtime:       blobInfo.Properties.LastModified,
				Size:        *blobInfo.Properties.ContentLength,
			}); err != nil {
				return err
			}
		}
		notDone = marker.NotDone()
	}

	a.mark = Mark{
		HasMore: false,
	}

	return nil
}

func (a *azureBlobWalker) Marker() Mark {
	return a.mark
}
