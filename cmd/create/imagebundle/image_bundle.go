// Copyright 2021 D2iQ, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package imagebundle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/distribution/distribution/v3/manifest/manifestlist"
	"github.com/mesosphere/dkp-cli/runtime/cli"
	"github.com/mesosphere/dkp-cli/runtime/cmd/log"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/klog/v2"

	"github.com/mesosphere/mindthegap/archive"
	"github.com/mesosphere/mindthegap/config"
	"github.com/mesosphere/mindthegap/docker/registry"
	"github.com/mesosphere/mindthegap/skopeo"
)

func NewCommand(ioStreams genericclioptions.IOStreams) *cobra.Command {
	var (
		configFile string
		platforms  []platform
		outputFile string
		overwrite  bool
	)

	cmd := &cobra.Command{
		Use: "image-bundle",
		RunE: func(cmd *cobra.Command, args []string) error {
			klog.SetOutput(ioStreams.ErrOut)
			logger := log.NewLogger(ioStreams.ErrOut)
			statusLogger := cli.StatusForLogger(logger)

			if !overwrite {
				statusLogger.Start("Checking if output file already exists")
				_, err := os.Stat(outputFile)
				switch {
				case err == nil:
					statusLogger.End(false)
					return fmt.Errorf("%s already exists: specify --overwrite to overwrite existing file", outputFile)
				case !errors.Is(err, os.ErrNotExist):
					statusLogger.End(false)
					return fmt.Errorf("failed to check if output file %s already exists: %w", outputFile, err)
				default:
					statusLogger.End(true)
				}
			}

			statusLogger.Start("Parsing image bundle config")
			cfg, err := config.ParseFile(configFile)
			if err != nil {
				statusLogger.End(false)
				return err
			}
			klog.V(4).Infof("Images config: %+v", cfg)
			statusLogger.End(true)

			statusLogger.Start("Creating temporary directory")
			outputFileAbs, err := filepath.Abs(outputFile)
			if err != nil {
				statusLogger.End(false)
				return fmt.Errorf("failed to determine where to create temporary directory: %w", err)
			}

			tempDir, err := os.MkdirTemp(filepath.Dir(outputFileAbs), ".image-bundle-*")
			if err != nil {
				statusLogger.End(false)
				return fmt.Errorf("failed to create temporary directory: %w", err)
			}
			defer os.RemoveAll(tempDir)
			statusLogger.End(true)

			statusLogger.Start("Starting temporary Docker registry")
			reg, err := registry.NewRegistry(registry.Config{StorageDirectory: tempDir})
			if err != nil {
				statusLogger.End(false)
				return fmt.Errorf("failed to create local Docker registry: %w", err)
			}
			go func() {
				if err := reg.ListenAndServe(); err != nil {
					fmt.Fprintf(ioStreams.ErrOut, "error serving Docker registry: %v\n", err)
					os.Exit(2)
				}
			}()
			statusLogger.End(true)

			skopeoRunner, skopeoCleanup := skopeo.NewRunner()
			defer func() { _ = skopeoCleanup() }()

			for registryName, registryConfig := range cfg {
				var skopeoOpts []skopeo.SkopeoOption
				if registryConfig.TLSVerify != nil && !*registryConfig.TLSVerify {
					skopeoOpts = append(skopeoOpts, skopeo.DisableSrcTLSVerify())
				}
				if registryConfig.Credentials != nil && registryConfig.Credentials.Username != "" {
					skopeoOpts = append(
						skopeoOpts,
						skopeo.SrcCredentials(registryConfig.Credentials.Username, registryConfig.Credentials.Password),
					)
				} else {
					err = skopeoRunner.AttemptToLoginToRegistry(context.TODO(), registryName, klog.V(4).Enabled())
					if err != nil {
						return fmt.Errorf("error logging in to registry: %w", err)
					}
				}
				if klog.V(4).Enabled() {
					skopeoOpts = append(skopeoOpts, skopeo.Debug())
				}

				for imageName, imageTags := range registryConfig.Images {
					for _, imageTag := range imageTags {
						srcImageName := fmt.Sprintf("%s/%s:%s", registryName, imageName, imageTag)
						statusLogger.Start(
							fmt.Sprintf("Copying %s (platforms: %v)",
								srcImageName, platforms,
							),
						)

						srcImageManifestList, skopeoOutput, err := skopeoRunner.InspectManifest(
							context.TODO(), fmt.Sprintf("docker://%s", srcImageName),
						)
						if err != nil {
							klog.Info(string(skopeoOutput))
							statusLogger.End(false)
							return err
						}
						klog.V(4).Info(string(skopeoOutput))
						destImageManifestList := manifestlist.ManifestList{Versioned: srcImageManifestList.Versioned}
						platformManifests := make(map[string]manifestlist.ManifestDescriptor, len(srcImageManifestList.Manifests))
						for _, m := range srcImageManifestList.Manifests {
							srcManifestPlatform := m.Platform.OS + "/" + m.Platform.Architecture
							if m.Platform.Variant != "" {
								srcManifestPlatform += "/" + m.Platform.Variant
							}
							platformManifests[srcManifestPlatform] = m
						}

						var digestFound bool
						for _, p := range platforms {
							platformManifest, ok := platformManifests[p.String()]
							if !ok {
								if p.arch == "arm64" {
									p.variant = "v8"
								}
								platformManifest, ok = platformManifests[p.String()]
								if !ok {
									klog.Warningf("could not find platform %s for image %s, continuing without a digest\n", p, srcImageName)
								}
							}
							// the digest may be empty, don't use it but still save images using tags
							digestFound = platformManifest.Digest != ""

							src := fmt.Sprintf("docker://%s/%s:%s", registryName, imageName, imageTag)
							dst := fmt.Sprintf("docker://%s/%s:%s", reg.Address(), imageName, imageTag)
							if digestFound {
								src = fmt.Sprintf("docker://%s/%s@%s", registryName, imageName, platformManifest.Digest)
								dst = fmt.Sprintf("docker://%s/%s@%s", reg.Address(), imageName, platformManifest.Digest)
							}

							skopeoOutput, err := skopeoRunner.Copy(context.TODO(),
								src,
								dst,
								append(
									skopeoOpts,
									skopeo.DisableDestTLSVerify(), skopeo.OS(p.os), skopeo.Arch(p.arch), skopeo.Variant(p.variant),
								)...,
							)
							if err != nil {
								klog.Info(string(skopeoOutput))
								statusLogger.End(false)
								return err
							}
							klog.V(4).Info(string(skopeoOutput))

							if digestFound {
								destImageManifestList.Manifests = append(destImageManifestList.Manifests, platformManifest)
							}
						}
						if digestFound {
							skopeoOutput, err = skopeoRunner.CopyManifest(context.TODO(),
								destImageManifestList,
								fmt.Sprintf("docker://%s/%s:%s", reg.Address(), imageName, imageTag),
								append(
									skopeoOpts,
									skopeo.DisableDestTLSVerify(),
								)...,
							)
							if err != nil {
								klog.Info(string(skopeoOutput))
								statusLogger.End(false)
								return err
							}
							klog.V(4).Info(string(skopeoOutput))
						}
						statusLogger.End(true)
					}
				}
			}

			if err := config.WriteSanitizedConfig(cfg, filepath.Join(tempDir, "images.yaml")); err != nil {
				return err
			}

			statusLogger.Start(fmt.Sprintf("Archiving images to %s", outputFile))
			if err := archive.ArchiveDirectory(tempDir, outputFile); err != nil {
				statusLogger.End(false)
				return fmt.Errorf("failed to create image bundle tarball: %w", err)
			}
			statusLogger.End(true)

			return nil
		},
	}

	cmd.Flags().StringVar(&configFile, "images-file", "", "YAML file containing list of images to create bundle from")
	_ = cmd.MarkFlagRequired("images-file")
	cmd.Flags().Var(newPlatformSlicesValue([]platform{{os: "linux", arch: "amd64"}}, &platforms), "platform",
		"platforms to download images (required format: <os>/<arch>[/<variant>])")
	cmd.Flags().StringVar(&outputFile, "output-file", "images.tar", "Output file to write image bundle to")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Overwrite image bundle file if it already exists")

	return cmd
}
