package atlas

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/hashicorp/atlas-go/archive"
	"github.com/hashicorp/atlas-go/v1"
	"github.com/mitchellh/mapstructure"
	"github.com/mitchellh/packer/common"
	"github.com/mitchellh/packer/packer"
)

const BuildEnvKey = "ATLAS_BUILD_ID"

// Artifacts can return a string for this state key and the post-processor
// will use automatically use this as the type. The user's value overrides
// this if `artifact_type_override` is set to true.
const ArtifactStateType = "atlas.artifact.type"

// Artifacts can return a map[string]string for this state key and this
// post-processor will automatically merge it into the metadata for any
// uploaded artifact versions.
const ArtifactStateMetadata = "atlas.artifact.metadata"

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	Artifact     string
	Type         string `mapstructure:"artifact_type"`
	TypeOverride bool   `mapstructure:"artifact_type_override"`
	Metadata     map[string]string

	ServerAddr string `mapstructure:"server_address"`
	Token      string

	// This shouldn't ever be set outside of unit tests.
	Test bool `mapstructure:"test"`

	tpl        *packer.ConfigTemplate
	user, name string
	buildId    int
}

type PostProcessor struct {
	config Config
	client *atlas.Client
}

func (p *PostProcessor) Configure(raws ...interface{}) error {
	_, err := common.DecodeConfig(&p.config, raws...)
	if err != nil {
		return err
	}

	p.config.tpl, err = packer.NewConfigTemplate()
	if err != nil {
		return err
	}
	p.config.tpl.UserVars = p.config.PackerUserVars

	templates := map[string]*string{
		"artifact":       &p.config.Artifact,
		"type":           &p.config.Type,
		"server_address": &p.config.ServerAddr,
		"token":          &p.config.Token,
	}

	errs := new(packer.MultiError)
	for key, ptr := range templates {
		*ptr, err = p.config.tpl.Process(*ptr, nil)
		if err != nil {
			errs = packer.MultiErrorAppend(
				errs, fmt.Errorf("Error processing %s: %s", key, err))
		}
	}

	required := map[string]*string{
		"artifact":      &p.config.Artifact,
		"artifact_type": &p.config.Type,
	}

	for key, ptr := range required {
		if *ptr == "" {
			errs = packer.MultiErrorAppend(
				errs, fmt.Errorf("%s must be set", key))
		}
	}

	if len(errs.Errors) > 0 {
		return errs
	}

	p.config.user, p.config.name, err = atlas.ParseSlug(p.config.Artifact)
	if err != nil {
		return err
	}

	// If we have a build ID, save it
	if v := os.Getenv(BuildEnvKey); v != "" {
		raw, err := strconv.ParseInt(v, 0, 0)
		if err != nil {
			return fmt.Errorf(
				"Error parsing build ID: %s", err)
		}

		p.config.buildId = int(raw)
	}

	// Build the client
	p.client = atlas.DefaultClient()
	if p.config.ServerAddr != "" {
		p.client, err = atlas.NewClient(p.config.ServerAddr)
		if err != nil {
			errs = packer.MultiErrorAppend(
				errs, fmt.Errorf("Error initializing atlas client: %s", err))
			return errs
		}
	}
	if p.config.Token != "" {
		p.client.Token = p.config.Token
	}

	if !p.config.Test {
		// Verify the client
		if err := p.client.Verify(); err != nil {
			if err == atlas.ErrAuth {
				errs = packer.MultiErrorAppend(
					errs, fmt.Errorf("Error connecting to atlas server, please check your ATLAS_TOKEN env: %s", err))
			} else {
				errs = packer.MultiErrorAppend(
					errs, fmt.Errorf("Error initializing atlas client: %s", err))
			}
			return errs
		}
	}

	return nil
}

func (p *PostProcessor) PostProcess(ui packer.Ui, artifact packer.Artifact) (packer.Artifact, bool, error) {
	if _, err := p.client.Artifact(p.config.user, p.config.name); err != nil {
		if err != atlas.ErrNotFound {
			return nil, false, fmt.Errorf(
				"Error finding artifact: %s", err)
		}

		// Artifact doesn't exist, create it
		ui.Message(fmt.Sprintf("Creating artifact: %s", p.config.Artifact))
		_, err = p.client.CreateArtifact(p.config.user, p.config.name)
		if err != nil {
			return nil, false, fmt.Errorf(
				"Error creating artifact: %s", err)
		}
	}

	opts := &atlas.UploadArtifactOpts{
		User:     p.config.user,
		Name:     p.config.name,
		Type:     p.config.Type,
		ID:       artifact.Id(),
		Metadata: p.metadata(artifact),
		BuildID:  p.config.buildId,
	}

	if fs := artifact.Files(); len(fs) > 0 {
		var archiveOpts archive.ArchiveOpts

		// We have files. We want to compress/upload them. If we have just
		// one file, then we use it as-is. Otherwise, we compress all of
		// them into a single file.
		var path string
		if len(fs) == 1 {
			path = fs[0]
		} else {
			path = longestCommonPrefix(fs)
			if path == "" {
				return nil, false, fmt.Errorf(
					"No common prefix for achiving files: %v", fs)
			}

			// Modify the archive options to only include the files
			// that are in our file list.
			include := make([]string, 0, len(fs))
			for i, f := range fs {
				include[i] = strings.Replace(f, path, "", 1)
			}
			archiveOpts.Include = include
		}

		r, err := archive.CreateArchive(path, &archiveOpts)
		if err != nil {
			return nil, false, fmt.Errorf(
				"Error archiving artifact: %s", err)
		}
		defer r.Close()

		opts.File = r
		opts.FileSize = r.Size
	}

	ui.Message("Uploading artifact version...")
	var av *atlas.ArtifactVersion
	doneCh := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		var err error
		av, err = p.client.UploadArtifact(opts)
		if err != nil {
			errCh <- err
			return
		}
		close(doneCh)
	}()

	select {
	case err := <-errCh:
		return nil, false, fmt.Errorf("Error uploading: %s", err)
	case <-doneCh:
	}

	return &Artifact{
		Name:    p.config.Artifact,
		Type:    p.config.Type,
		Version: av.Version,
	}, true, nil
}

func (p *PostProcessor) metadata(artifact packer.Artifact) map[string]string {
	var metadata map[string]string
	metadataRaw := artifact.State(ArtifactStateMetadata)
	if metadataRaw != nil {
		if err := mapstructure.Decode(metadataRaw, &metadata); err != nil {
			panic(err)
		}
	}

	if p.config.Metadata != nil {
		// If we have no extra metadata, just return as-is
		if metadata == nil {
			return p.config.Metadata
		}

		// Merge the metadata
		for k, v := range p.config.Metadata {
			metadata[k] = v
		}
	}

	return metadata
}

func (p *PostProcessor) artifactType(artifact packer.Artifact) string {
	if !p.config.TypeOverride {
		if v := artifact.State(ArtifactStateType); v != nil {
			return v.(string)
		}
	}

	return p.config.Type
}
