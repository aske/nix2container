// This package can be used to generate image configuration and blobs
// from an image JSON file.
//
// First, you need to create an types.Image with NewImageFromFile.
//
// With this types.Image object, it is then possible to get the image
// configuration with the GetConfigBlob method. To get layer blobs,
// you need to iterate on the layers of the image and use the GetBlob
// or LayerGetBlob functions to get a Reader on this layer.
package nix

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/containers/image/v5/manifest"
	"github.com/nlewo/nix2container/types"
	godigest "github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

// GetConfigBlob returns the config blog of an image.
func GetConfigBlob(image types.Image) ([]byte, error) {
	imageV1, err := getV1Image(image)
	if err != nil {
		return nil, err
	}
	configBlob, err := json.Marshal(imageV1)
	if err != nil {
		return nil, err
	}
	return configBlob, nil
}

// GetConfigDigest returns the digest and the size of the config blog of an image.
func GetConfigDigest(image types.Image) (d godigest.Digest, size int64, err error) {
	configBlob, err := GetConfigBlob(image)
	if err != nil {
		return d, size, err
	}
	d = godigest.FromBytes(configBlob)
	return d, int64(len(configBlob)), err
}

// GetBlob gets the layer corresponding to the provided digest.
func GetBlob(image types.Image, digest godigest.Digest) (io.ReadCloser, int64, error) {
	for _, layer := range image.Layers {
		if layer.Digest == digest.String() {
			return LayerGetBlob(layer)
		}
	}
	configDigest, _, err := GetConfigDigest(image)
	if err != nil {
		return nil, 0, err
	}
	if digest == configDigest {
		configBlob, err := GetConfigBlob(image)
		if err != nil {
			return nil, 0, err
		}
		rc := nopCloser{bytes.NewReader(configBlob)}
		return rc, int64(len(configBlob)), nil
	}
	return nil, 0, errors.New("No blob with specified digest found in image")
}

func getV1Image(image types.Image) (imageV1 v1.Image, err error) {
	imageV1.OS = "linux"
	imageV1.Architecture = image.Arch
	imageV1.Config = image.ImageConfig
	imageV1.Created = image.Created

	for _, layer := range image.Layers {
		digest, err := godigest.Parse(layer.DiffIDs)
		if err != nil {
			return imageV1, err
		}
		imageV1.RootFS.DiffIDs = append(
			imageV1.RootFS.DiffIDs,
			digest)
		imageV1.RootFS.Type = "layers"
		// Even if optional in the spec, we
		// need to add an history otherwise
		// some toolings can complain:
		// https://github.com/nlewo/nix2container/issues/57
		imageV1.History = append(
			imageV1.History,
			layer.History,
		)
	}
	return
}

// NewImageFromFile creates an Image from a JSON file describing an
// image. This file has usually been created by Nix through the
// nix2container binary.
func NewImageFromFile(filename string) (image types.Image, err error) {
	file, err := os.Open(filename)
	if err != nil {
		return image, err
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		return image, err
	}
	err = json.Unmarshal(content, &image)
	if err != nil {
		return image, err
	}
	return image, nil
}

// NewImageFromDir builds an Image based on an directory populated by
// the Skopeo dir transport. The directory needs to be a absolute
// path since tarball filepaths are referenced in the image Layers.
func NewImageFromDir(directory string) (image types.Image, err error) {
	image.Version = types.ImageVersion

	manifestFile, err := os.Open(directory + "/manifest.json")
	if err != nil {
		return image, err
	}
	defer manifestFile.Close()
	content, err := io.ReadAll(manifestFile)
	if err != nil {
		return image, err
	}
	var v1Manifest v1.Manifest
	err = json.Unmarshal(content, &v1Manifest)
	if err != nil {
		return image, err
	}

	content, err = os.ReadFile(directory + "/" + v1Manifest.Config.Digest.Encoded())
	if err != nil {
		return image, err
	}
	var v1ImageConfig manifest.Schema2Image
	err = json.Unmarshal(content, &v1ImageConfig)
	if err != nil {
		return image, err
	}

	{
		configLayerFileName := directory + "/" + v1Manifest.Config.Digest.Encoded()
		logrus.Infof("Loading image config from '%s'", configLayerFileName)
		configLayerFile, err := os.Open(configLayerFileName)
		if err != nil {
			return image, err
		}
		configLayerContent, err := io.ReadAll(configLayerFile)
		if err != nil {
			return image, err
		}
		var v1Image v1.Image
		err = json.Unmarshal(configLayerContent, &v1Image)
		if err != nil {
			return image, err
		}
		image.ImageConfig = v1Image.Config
	}

	for i, l := range v1Manifest.Layers {
		layerFilename := directory + "/" + l.Digest.Encoded()
		logrus.Infof("Adding tar file '%s' as image layer", layerFilename)
		
		if i >= len(v1ImageConfig.RootFS.DiffIDs) {
			return image, errors.New("mismatch between number of layers and DiffIDs")
		}
		
		layer := types.Layer{
			LayerPath: layerFilename,
			Digest:    l.Digest.String(),
			DiffIDs:   v1ImageConfig.RootFS.DiffIDs[i].String(),
		}
		err = layer.SetMediaTypeFromDescriptor(l)
		if err != nil {
			return image, err
		}
		image.Layers = append(image.Layers, layer)
	}
	return image, nil
}

// NewImageFromManifest builds an Image based on a registry manifest
// and a separate JSON mapping pointing to the locations of the
// associated blobs (layer archives).
func NewImageFromManifest(manifestFilename string, blobMapFilename string) (image types.Image, err error) {
	image.Version = types.ImageVersion

	content, err := os.ReadFile(manifestFilename)
	if err != nil {
		return image, err
	}
	var v1Manifest v1.Manifest
	err = json.Unmarshal(content, &v1Manifest)
	if err != nil {
		return image, err
	}

	var blobMap map[string]string
	content, err = os.ReadFile(blobMapFilename)
	if err != nil {
		return image, err
	}
	err = json.Unmarshal(content, &blobMap)
	if err != nil {
		return image, err
	}

	var configFilename = blobMap[v1Manifest.Config.Digest.Encoded()]
	content, err = os.ReadFile(configFilename)
	if err != nil {
		return image, err
	}
	var v1ImageConfig manifest.Schema2Image
	err = json.Unmarshal(content, &v1ImageConfig)
	if err != nil {
		return image, err
	}

	for i, l := range v1Manifest.Layers {
		layerFilename := blobMap[l.Digest.Encoded()]
		logrus.Infof("Adding tar file '%s' as image layer", layerFilename)
		
		if i >= len(v1ImageConfig.RootFS.DiffIDs) {
			return image, errors.New("mismatch between number of layers and DiffIDs")
		}
		
		layer := types.Layer{
			LayerPath: layerFilename,
			Digest:    l.Digest.String(),
			DiffIDs:   v1ImageConfig.RootFS.DiffIDs[i].String(),
		}
		err = layer.SetMediaTypeFromDescriptor(l)
		if err != nil {
			return image, err
		}
		image.Layers = append(image.Layers, layer)
	}
	return image, nil
}

type nopCloser struct {
	io.Reader
}

func (nopCloser) Close() error { return nil }

func MergeOtherImageConfig(target *v1.ImageConfig, other *v1.ImageConfig) {
	// User: overwrite
	if len(other.User) > 0 {
		target.User = other.User
	}

	// ExposedPorts: join
	if target.ExposedPorts == nil {
		target.ExposedPorts = make(map[string]struct{})
	}
	for k, v := range other.ExposedPorts {
		target.ExposedPorts[k] = v
	}

	// Env: join
	if target.Env == nil {
		target.Env = make([]string, 0)
	}
	target.Env = append(target.Env, other.Env...)

	// Entrypoint: overwrite
	if len(other.Entrypoint) > 0 {
		target.Entrypoint = other.Entrypoint
	}

	// Cmd: overwrite
	if len(other.Cmd) > 0 {
		target.Cmd = other.Cmd
	}

	// Volumes: join
	if target.Volumes == nil {
		target.Volumes = make(map[string]struct{})
	}
	for k, v := range other.Volumes {
		target.Volumes[k] = v
	}

	// WorkingDir: overwrite
	if len(other.WorkingDir) > 0 {
		target.WorkingDir = other.WorkingDir
	}

	// Labels: join
	if target.Labels == nil {
		target.Labels = make(map[string]string)
	}
	for k, v := range other.Labels {
		target.Labels[k] = v
	}

	// StopSignal: overwrite
	if len(other.StopSignal) > 0 {
		target.StopSignal = other.StopSignal
	}
}
