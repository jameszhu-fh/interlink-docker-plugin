package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	exec2 "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	commonIL "github.com/intertwin-eu/interlink-docker-plugin/pkg/common"
	"github.com/intertwin-eu/interlink-docker-plugin/pkg/docker/dindmanager"
	"github.com/intertwin-eu/interlink-docker-plugin/pkg/docker/fpgastrategies"
	"github.com/intertwin-eu/interlink-docker-plugin/pkg/docker/gpustrategies"
)

type SidecarHandler struct {
	Config      commonIL.InterLinkConfig
	Ctx         context.Context
	GpuManager  gpustrategies.GPUManagerInterface
	DindManager dindmanager.DindManagerInterface
	FPGAManager fpgastrategies.FPGAManagerInterface
}

func escapeDockerArgs(args []string) []string {
	escapedArgs := make([]string, 0, len(args))
	for _, arg := range args {
		if strings.ContainsAny(arg, " &;*$") {
			arg = `"` + strings.ReplaceAll(arg, `"`, `\"`) + `"`
		}
		escapedArgs = append(escapedArgs, arg)
	}
	return escapedArgs
}

func parseContainerCommandAndReturnArgs(Ctx context.Context, config commonIL.InterLinkConfig, podUID string, podNamespace string, container v1.Container) ([]string, []string, []string, error) {

	dirPath := config.DataRootFolder + podNamespace + "-" + podUID
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		err := os.MkdirAll(dirPath, os.ModePerm)
		if err != nil {
			log.G(Ctx).Error(err)
		} else {
			log.G(Ctx).Info("-- Created directory " + dirPath)
		}
	}

	if container.Command == nil {
		return []string{}, container.Command, container.Args, nil
	}

	prefileName := container.Name + "_" + podUID + "_" + podNamespace

	wd, err := os.Getwd()
	if err != nil {
		return nil, nil, nil, err
	}

	if len(container.Command) > 0 {

		fileName := prefileName + "_script.sh"

		if len(container.Args) == 0 {
			fileNamePath := filepath.Join(wd, config.DataRootFolder+podNamespace+"-"+podUID, fileName)
			err = os.WriteFile(fileNamePath, []byte(strings.Join(container.Command, " ")), 0644)
			if err != nil {
				log.G(Ctx).Error(err)
				return nil, nil, nil, err
			}
			return []string{"-v " + fileNamePath + ":/" + fileName}, []string{"/bin/sh" + " /" + fileName}, []string{}, nil
		}

		argsFileName := container.Name + "_args"
		argsFileNamePath := filepath.Join(wd, config.DataRootFolder+podNamespace+"-"+podUID, argsFileName)
		err = os.WriteFile(argsFileNamePath, []byte(strings.Join(container.Args, " ")), 0644)
		if err != nil {
			log.G(Ctx).Error(err)
			return nil, nil, nil, err
		}

		fullFileContent := strings.Join(container.Command, " ") + " \"$(cat " + argsFileName + ")\""
		fullFileNamePath := filepath.Join(wd, config.DataRootFolder+podNamespace+"-"+podUID, fileName)
		err = os.WriteFile(fullFileNamePath, []byte(fullFileContent), 0644)
		if err != nil {
			log.G(Ctx).Error(err)
			return nil, nil, nil, err
		}

		return []string{"-v " + argsFileNamePath + ":/" + argsFileName, "-v " + fullFileNamePath + ":/" + fileName}, []string{"/bin/sh" + " /" + fileName}, []string{}, nil

	} else {
		return []string{}, container.Command, container.Args, nil
	}
}

func prepareMounts(Ctx context.Context, config commonIL.InterLinkConfig, data commonIL.RetrievedPodData, container v1.Container) (string, error) {
	mountedData := ""

	podUID := string(data.Pod.UID)
	podNamespace := string(data.Pod.UID)

	err := os.MkdirAll(config.DataRootFolder+data.Pod.Namespace+"-"+podUID, os.ModePerm)
	if err != nil {
		return "", err
	}

	allContainers := append(data.Containers, data.InitContainers...)

	for _, cont := range allContainers {

		if cont.Name != container.Name {
			continue
		}

		containerName := podNamespace + "-" + podUID + "-" + container.Name

		for _, cfgMap := range cont.ConfigMaps {
			if containerName == podNamespace+"-"+podUID+"-"+cont.Name {
				paths, err := mountData(Ctx, config, data.Pod, cfgMap, container)
				if err != nil {
					return "", errors.New("Error mounting ConfigMap " + cfgMap.Name)
				}
				for _, path := range paths {
					mountedData += "-v " + path + " "
				}
			}
		}

		for _, secret := range cont.Secrets {
			if containerName == podNamespace+"-"+podUID+"-"+cont.Name {
				paths, err := mountData(Ctx, config, data.Pod, secret, container)
				if err != nil {
					return "", errors.New("Error mounting Secret " + secret.Name)
				}
				for _, path := range paths {
					mountedData += "-v " + path + " "
				}
			}
		}

		for _, emptyDir := range cont.EmptyDirs {
			if containerName == podNamespace+"-"+podUID+"-"+cont.Name {
				paths, err := mountData(Ctx, config, data.Pod, emptyDir, container)
				if err != nil {
					log.G(Ctx).Error("Error mounting EmptyDir " + emptyDir)
					return "", errors.New("Error mounting EmptyDir " + emptyDir)
				}
				for _, path := range paths {
					mountedData += "-v " + path + " "
				}
			}
		}
	}

	if last := len(mountedData) - 1; last >= 0 && mountedData[last] == ',' {
		mountedData = mountedData[:last]
	}
	return mountedData, nil
}

func mountData(Ctx context.Context, config commonIL.InterLinkConfig, pod v1.Pod, data interface{}, container v1.Container) ([]string, error) {
	wd, err := os.Getwd()
	if err != nil {
		log.G(Ctx).Error(err)
		return nil, err
	}

	for _, mountSpec := range container.VolumeMounts {

		var podVolumeSpec *v1.VolumeSource

		for _, vol := range pod.Spec.Volumes {
			if vol.Name == mountSpec.Name {
				podVolumeSpec = &vol.VolumeSource
			}

			switch mount := data.(type) {
			case v1.ConfigMap:
				var configMapNamePaths []string
				err := os.RemoveAll(config.DataRootFolder + pod.Namespace + "-" + string(pod.UID) + "/" + "configMaps/" + vol.Name)

				if err != nil {
					return nil, err
				}

				if podVolumeSpec != nil && podVolumeSpec.ConfigMap != nil {
					podConfigMapDir := filepath.Join(wd+"/"+config.DataRootFolder+pod.Namespace+"-"+string(pod.UID)+"/", "configMaps/", vol.Name)
					mode := os.FileMode(*podVolumeSpec.ConfigMap.DefaultMode)

					correctMountPath := ""
					for _, volumeMount := range container.VolumeMounts {
						if volumeMount.Name == vol.Name {
							correctMountPath = volumeMount.MountPath
						}
					}

					if mount.Data != nil {
						for key := range mount.Data {
							path := filepath.Join(podConfigMapDir, key)
							path += (":" + correctMountPath + "/" + key + " ")
							configMapNamePaths = append(configMapNamePaths, path)
						}
					}

					cmd := []string{"-p " + podConfigMapDir}
					shell := exec2.ExecTask{
						Command: "mkdir",
						Args:    cmd,
						Shell:   true,
					}

					execReturn, _ := shell.Execute()
					if execReturn.Stderr != "" {
						return nil, err
					}

					for k, v := range mount.Data {
						// TODO: Ensure that these files are deleted in failure cases
						fullPath := filepath.Join(podConfigMapDir, k)
						os.WriteFile(fullPath, []byte(v), mode)
						if err != nil {
							err = os.RemoveAll(fullPath)
							return nil, err
						}
					}
					return configMapNamePaths, nil
				}

			case v1.Secret:
				var secretNamePaths []string
				err := os.RemoveAll(config.DataRootFolder + pod.Namespace + "-" + string(pod.UID) + "/" + "secrets/" + vol.Name)

				if err != nil {
					return nil, err
				}
				if podVolumeSpec != nil && podVolumeSpec.Secret != nil {
					mode := os.FileMode(*podVolumeSpec.Secret.DefaultMode)
					podSecretDir := filepath.Join(wd+"/"+config.DataRootFolder+pod.Namespace+"-"+string(pod.UID)+"/", "secrets/", vol.Name)

					if mount.Data != nil {
						for key := range mount.Data {
							path := filepath.Join(podSecretDir, key)
							path += (":" + mountSpec.MountPath + "/" + key + " ")
							secretNamePaths = append(secretNamePaths, path)
						}
					}

					cmd := []string{"-p " + podSecretDir}
					shell := exec2.ExecTask{
						Command: "mkdir",
						Args:    cmd,
						Shell:   true,
					}

					execReturn, _ := shell.Execute()
					if strings.Compare(execReturn.Stdout, "") != 0 {
						return nil, err
					}
					if execReturn.Stderr != "" {
						return nil, errors.New(execReturn.Stderr)
					}

					for k, v := range mount.Data {
						// TODO: Ensure that these files are deleted in failure cases
						fullPath := filepath.Join(podSecretDir, k)
						os.WriteFile(fullPath, v, mode)
						if err != nil {
							err = os.RemoveAll(fullPath)
							return nil, err
						}
					}
					return secretNamePaths, nil
				}

			case string:
				if podVolumeSpec != nil && podVolumeSpec.EmptyDir != nil {
					var edPath string

					parts := strings.Split(data.(string), "/")
					emptyDirName := parts[len(parts)-1]
					if emptyDirName != vol.Name {
						continue
					}

					emptyDirMountPath := ""
					isReadOnly := false
					isBidirectional := false

					for _, mountSpec := range container.VolumeMounts {
						if mountSpec.Name == vol.Name {
							emptyDirMountPath = mountSpec.MountPath
							if mountSpec.ReadOnly {
								isReadOnly = true
							}
							if mountSpec.MountPropagation != nil && *mountSpec.MountPropagation == v1.MountPropagationBidirectional {
								isBidirectional = true
							}

							break
						}
					}

					edPath = filepath.Join(wd + "/" + config.DataRootFolder + pod.Namespace + "-" + string(pod.UID) + "/" + "emptyDirs/" + vol.Name)
					cmd := []string{"-p " + edPath}
					shell := exec2.ExecTask{
						Command: "mkdir",
						Args:    cmd,
						Shell:   true,
					}

					_, err := shell.Execute()
					if err != nil {
						return []string{""}, nil
					}

					if isReadOnly {
						edPath += (":" + emptyDirMountPath + "/:ro")
					} else {
						if isBidirectional {
							edPath += (":" + emptyDirMountPath + "/:shared")
						} else {
							edPath += (":" + emptyDirMountPath + "/")
						}
					}

					return []string{edPath}, nil
				}
			}
		}
	}

	return nil, err
}
