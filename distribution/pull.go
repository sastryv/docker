package distribution

import (
	"fmt"
	"os"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/daemon/events"
	"github.com/docker/docker/distribution/metadata"
	"github.com/docker/docker/distribution/xfer"
	"github.com/docker/docker/image"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/registry"
	"github.com/docker/docker/tag"
	"golang.org/x/net/context"
)

// ImagePullConfig stores pull configuration.
type ImagePullConfig struct {
	// MetaHeaders stores HTTP headers with metadata about the image
	// (DockerHeaders with prefix X-Meta- in the request).
	MetaHeaders map[string][]string
	// AuthConfig holds authentication credentials for authenticating with
	// the registry.
	AuthConfig *types.AuthConfig
	// ProgressOutput is the interface for showing the status of the pull
	// operation.
	ProgressOutput progress.Output
	// RegistryService is the registry service to use for TLS configuration
	// and endpoint lookup.
	RegistryService *registry.Service
	// EventsService is the events service to use for logging.
	EventsService *events.Events
	// MetadataStore is the storage backend for distribution-specific
	// metadata.
	MetadataStore metadata.Store
	// ImageStore manages images.
	ImageStore image.Store
	// TagStore manages tags.
	TagStore tag.Store
	// DownloadManager manages concurrent pulls.
	DownloadManager *xfer.LayerDownloadManager
}

// Puller is an interface that abstracts pulling for different API versions.
type Puller interface {
	// Pull tries to pull the image referenced by `tag`
	// Pull returns an error if any, as well as a boolean that determines whether to retry Pull on the next configured endpoint.
	//
	Pull(ctx context.Context, ref reference.Named) (fallback bool, err error)
}

// newPuller returns a Puller interface that will pull from either a v1 or v2
// registry. The endpoint argument contains a Version field that determines
// whether a v1 or v2 puller will be created. The other parameters are passed
// through to the underlying puller implementation for use during the actual
// pull operation.
func newPuller(endpoint registry.APIEndpoint, repoInfo *registry.RepositoryInfo, imagePullConfig *ImagePullConfig) (Puller, error) {
	switch endpoint.Version {
	case registry.APIVersion2:
		return &v2Puller{
			blobSumService: metadata.NewBlobSumService(imagePullConfig.MetadataStore),
			endpoint:       endpoint,
			config:         imagePullConfig,
			repoInfo:       repoInfo,
		}, nil
	case registry.APIVersion1:
		return &v1Puller{
			v1IDService: metadata.NewV1IDService(imagePullConfig.MetadataStore),
			endpoint:    endpoint,
			config:      imagePullConfig,
			repoInfo:    repoInfo,
		}, nil
	}
	return nil, fmt.Errorf("unknown version %d for registry %s", endpoint.Version, endpoint.URL)
}

// Pull initiates a pull operation. image is the repository name to pull, and
// tag may be either empty, or indicate a specific tag to pull.
func Pull(ctx context.Context, ref reference.Named, imagePullConfig *ImagePullConfig) error {
	// Resolve the Repository name from fqn to RepositoryInfo
	repoInfo, err := imagePullConfig.RegistryService.ResolveRepository(ref)
	if err != nil {
		return err
	}

	// makes sure name is not empty or `scratch`
	if err := validateRepoName(repoInfo.LocalName.Name()); err != nil {
		return err
	}

	endpoints, err := imagePullConfig.RegistryService.LookupPullEndpoints(repoInfo.CanonicalName)
	if err != nil {
		return err
	}

	logName := registry.NormalizeLocalReference(ref)

	var (
		// use a slice to append the error strings and return a joined string to caller
		errors []string

		// discardNoSupportErrors is used to track whether an endpoint encountered an error of type registry.ErrNoSupport
		// By default it is false, which means that if a ErrNoSupport error is encountered, it will be saved in errors.
		// As soon as another kind of error is encountered, discardNoSupportErrors is set to true, avoiding the saving of
		// any subsequent ErrNoSupport errors in errors.
		// It's needed for pull-by-digest on v1 endpoints: if there are only v1 endpoints configured, the error should be
		// returned and displayed, but if there was a v2 endpoint which supports pull-by-digest, then the last relevant
		// error is the ones from v2 endpoints not v1.
		discardNoSupportErrors bool
	)
	for _, endpoint := range endpoints {
		logrus.Debugf("Trying to pull %s from %s %s", repoInfo.LocalName, endpoint.URL, endpoint.Version)

		puller, err := newPuller(endpoint, repoInfo, imagePullConfig)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}
		if fallback, err := puller.Pull(ctx, ref); err != nil {
			// Was this pull cancelled? If so, don't try to fall
			// back.
			select {
			case <-ctx.Done():
				fallback = false
			default:
			}
			if fallback {
				if _, ok := err.(registry.ErrNoSupport); !ok {
					// Because we found an error that's not ErrNoSupport, discard all subsequent ErrNoSupport errors.
					discardNoSupportErrors = true
					// append subsequent errors
					errors = append(errors, err.Error())
				} else if !discardNoSupportErrors {
					// Save the ErrNoSupport error, because it's either the first error or all encountered errors
					// were also ErrNoSupport errors.
					// append subsequent errors
					errors = append(errors, err.Error())
				}
				continue
			}
			errors = append(errors, err.Error())
			logrus.Debugf("Not continuing with error: %v", fmt.Errorf(strings.Join(errors, "\n")))
			if len(errors) > 0 {
				return fmt.Errorf(strings.Join(errors, "\n"))
			}
		}

		imagePullConfig.EventsService.Log("pull", logName.String(), "")
		return nil
	}

	if len(errors) == 0 {
		return fmt.Errorf("no endpoints found for %s", ref.String())
	}

	if len(errors) > 0 {
		return fmt.Errorf(strings.Join(errors, "\n"))
	}
	return nil
}

// writeStatus writes a status message to out. If layersDownloaded is true, the
// status message indicates that a newer image was downloaded. Otherwise, it
// indicates that the image is up to date. requestedTag is the tag the message
// will refer to.
func writeStatus(requestedTag string, out progress.Output, layersDownloaded bool) {
	if layersDownloaded {
		progress.Message(out, "", "Status: Downloaded newer image for "+requestedTag)
	} else {
		progress.Message(out, "", "Status: Image is up to date for "+requestedTag)
	}
}

// validateRepoName validates the name of a repository.
func validateRepoName(name string) error {
	if name == "" {
		return fmt.Errorf("Repository name can't be empty")
	}
	if name == "scratch" {
		return fmt.Errorf("'scratch' is a reserved name")
	}
	return nil
}

// tmpFileClose creates a closer function for a temporary file that closes the file
// and also deletes it.
func tmpFileCloser(tmpFile *os.File) func() error {
	return func() error {
		tmpFile.Close()
		if err := os.RemoveAll(tmpFile.Name()); err != nil {
			logrus.Errorf("Failed to remove temp file: %s", tmpFile.Name())
		}

		return nil
	}
}
