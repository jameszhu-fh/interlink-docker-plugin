package docker

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	exec "github.com/alexellis/go-execute/pkg/v1"
	"github.com/containerd/containerd/log"
	v1 "k8s.io/api/core/v1"

	"errors"

	commonIL "github.com/intertwin-eu/interlink-docker-plugin/pkg/common"
	"github.com/intertwin-eu/interlink-docker-plugin/pkg/docker/dindmanager"

	"path/filepath"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	trace "go.opentelemetry.io/otel/trace"
)

func (h *SidecarHandler) prepareDockerRuns(podData commonIL.RetrievedPodData, w http.ResponseWriter, podIp string) ([]DockerRunStruct, error) {

	var dockerRunStructs []DockerRunStruct
	var gpuArgs string = ""
	var fpgaArgs string = ""

	podUID := string(podData.Pod.UID)
	podNamespace := string(podData.Pod.Namespace)

	pathsOfVolumes := make(map[string]string)

	for _, volume := range podData.Pod.Spec.Volumes {
		if volume.HostPath != nil {
			if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate || *volume.HostPath.Type == v1.HostPathDirectory {
				_, err := os.Stat(volume.HostPath.Path)
				if *volume.HostPath.Type == v1.HostPathDirectory {
					if os.IsNotExist(err) {
						HandleErrorAndRemoveData(h, w, "Host path directory does not exist", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("Host path directory does not exist")
					}
					pathsOfVolumes[volume.Name] = volume.HostPath.Path
				} else if *volume.HostPath.Type == v1.HostPathDirectoryOrCreate {
					if os.IsNotExist(err) {
						err = os.MkdirAll(volume.HostPath.Path, os.ModePerm)
						if err != nil {
							HandleErrorAndRemoveData(h, w, "An error occurred during mkdir of host path directory", err, podNamespace, podUID)
							return dockerRunStructs, errors.New("An error occurred during mkdir of host path directory")
						} else {
							pathsOfVolumes[volume.Name] = volume.HostPath.Path
						}
					} else {
						pathsOfVolumes[volume.Name] = volume.HostPath.Path
					}
				}
			}
		}

		if volume.PersistentVolumeClaim != nil {
			if _, ok := pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName]; !ok {
				// WIP: this is a temporary solution to mount CVMFS volumes for persistent volume claims case
				pathsOfVolumes[volume.PersistentVolumeClaim.ClaimName] = "/cvmfs"
			}

		}
	}

	allContainers := map[string][]v1.Container{
		"initContainers": podData.Pod.Spec.InitContainers,
		"containers":     podData.Pod.Spec.Containers,
	}

	for containerType, containers := range allContainers {
		isInitContainer := containerType == "initContainers"

		for _, container := range containers {

			containerName := podNamespace + "-" + podUID + "-" + container.Name

			var isGpuRequested bool = false
			var isFPGARequested bool = false
			var additionalGpuArgs []string

			if val, ok := container.Resources.Limits["nvidia.com/gpu"]; ok {

				numGpusRequested := val.Value()

				// if the container is requesting 0 GPU, skip the GPU assignment
				if numGpusRequested == 0 {
					log.G(h.Ctx).Info("\u2705 Container " + containerName + " is not requesting a GPU")
				} else {

					log.G(h.Ctx).Info("\u2705 Container " + containerName + " is requesting " + val.String() + " GPU")

					isGpuRequested = true

					numGpusRequestedInt := int(numGpusRequested)
					_, err := h.GpuManager.GetAvailableGPUs(numGpusRequestedInt)

					if err != nil {
						HandleErrorAndRemoveData(h, w, "An error occurred during request of get available GPUs", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("An error occurred during request of get available GPUs")
					}

					gpuSpecs, err := h.GpuManager.GetAndAssignAvailableGPUs(numGpusRequestedInt, containerName)
					if err != nil {
						HandleErrorAndRemoveData(h, w, "An error occurred during request of get and assign of an available GPU", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("An error occurred during request of get and assign of an available GPU")
					}

					var gpuUUIDs string = ""
					for _, gpuSpec := range gpuSpecs {
						if gpuSpec.UUID == gpuSpecs[len(gpuSpecs)-1].UUID {
							gpuUUIDs += strconv.Itoa(gpuSpec.Index)
						} else {
							gpuUUIDs += strconv.Itoa(gpuSpec.Index) + ","
						}
					}

					gpuArgs = "--gpus '\"device=" + gpuUUIDs + "\"'"
					additionalGpuArgs = append(additionalGpuArgs, gpuArgs)
				}

			}

			if val, ok := container.Resources.Limits["xilinx.com/fpga"]; ok {
				numFPGAsRequested := val.Value()
				if numFPGAsRequested == 0 {
					log.G(h.Ctx).Info("\u2705 Container " + containerName + " is not requesting a FPGA")
				} else {

					isFPGARequested = true
					log.G(h.Ctx).Info("\u2705 Container " + containerName + " is requesting " + strconv.Itoa(int(numFPGAsRequested)) + " FPGA(s)")

					numFPGAsRequestedInt := int(numFPGAsRequested)
					_, err := h.FPGAManager.GetAvailableFPGAs(numFPGAsRequestedInt)
					if err != nil {
						HandleErrorAndRemoveData(h, w, "An error occurred during the request of available FPGAs", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("An error occurred during the request of available FPGAs")
					}
					assignedFPGAs, err := h.FPGAManager.GetAndAssignAvailableFPGAs(numFPGAsRequestedInt, containerName)
					if err != nil {
						HandleErrorAndRemoveData(h, w, "An error occurred during request of get and assign of an available GPU", err, podNamespace, podUID)
						return dockerRunStructs, errors.New("An error occurred during request of get and assign of an available GPU")
					}
					for _, fpgaSpec := range assignedFPGAs {
						fpgaArgs += " --device=" + fpgaSpec.DeviceToMount + ":" + fpgaSpec.DeviceToMount
					}
				}
			}

			var envVars string
			for _, envVar := range container.Env {
				if envVar.Value != "" {
					value := envVar.Value

					// If the value starts with a double quote followed by a bracket,
					// remove the outer double quotes before wrapping.
					if strings.HasPrefix(value, "\"[") && strings.HasSuffix(value, "]\"") {
						// Remove the first and last character.
						value = value[1 : len(value)-1]
					}

					// Now, if the value looks like a list (starts with '['), wrap it with single quotes.
					if strings.HasPrefix(value, "[") {
						envVars += " -e " + envVar.Name + "='" + value + "'"
					} else if strings.Contains(value, " ") {
						// For values containing spaces, wrap in double quotes.
						envVars += " -e " + envVar.Name + "=\"" + value + "\""
					} else {
						envVars += " -e " + envVar.Name + "=" + value
					}
				} else {
					envVars += " -e " + envVar.Name
				}
			}

			for _, volumeMount := range container.VolumeMounts {
				if volumeMount.MountPath != "" {

					if _, ok := pathsOfVolumes[volumeMount.Name]; !ok {
						continue
					}
					if volumeMount.ReadOnly {
						envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath + ":ro"
					} else {
						if volumeMount.MountPropagation != nil && *volumeMount.MountPropagation == v1.MountPropagationBidirectional {
							envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath + ":shared"
						} else {
							envVars += " -v " + pathsOfVolumes[volumeMount.Name] + ":" + volumeMount.MountPath
						}
					}
				}
			}

			//envVars += " --network=host"
			cmd := []string{"run", "-d", "--name", containerName}

			cmd = append(cmd, envVars)

			if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
				cmd = append(cmd, "--privileged")
			}

			if isGpuRequested {
				cmd = append(cmd, additionalGpuArgs...)
			}

			if isFPGARequested {
				cmd = append(cmd, fpgaArgs)
			}

			cmd = append(cmd, "-p", "8888:8888")

			// if podIp != "" {
			// 	// add --ip flag to the docker run command
			// 	cmd = append(cmd, "--ip", podIp)

			// 	// add --net vk0
			// 	cmd = append(cmd, "--net", "vk0")

			// 	// --dns 10.96.0.10
			// 	cmd = append(cmd, "--dns", "10.96.0.10")

			// 	// add NET_ADMIN  capability
			// 	cmd = append(cmd, "--cap-add", "NET_ADMIN")
			// }

			var additionalPortArgs []string

			for _, port := range container.Ports {
				if port.HostPort != 0 {
					additionalPortArgs = append(additionalPortArgs, "-p", strconv.Itoa(int(port.HostPort))+":"+strconv.Itoa(int(port.ContainerPort)))
				}
			}

			cmd = append(cmd, additionalPortArgs...)

			mounts, err := prepareMounts(h.Ctx, h.Config, podData, container)
			if err != nil {
				HandleErrorAndRemoveData(h, w, "An error occurred during preparing mounts for the POD", err, podNamespace, podUID)
				return dockerRunStructs, errors.New("An error occurred during preparing mounts for the POD")
			}

			cmd = append(cmd, mounts)

			memoryLimitsArray := []string{}
			cpuLimitsArray := []string{}

			if container.Resources.Limits.Memory().Value() != 0 {
				memoryLimitsArray = append(memoryLimitsArray, "--memory", strconv.Itoa(int(container.Resources.Limits.Memory().Value()))+"b")
			}
			if container.Resources.Limits.Cpu().Value() != 0 {
				cpuLimitsArray = append(cpuLimitsArray, "--cpus", strconv.FormatFloat(float64(container.Resources.Limits.Cpu().Value()), 'f', -1, 64))
			}

			cmd = append(cmd, memoryLimitsArray...)
			cmd = append(cmd, cpuLimitsArray...)

			containerCommands := []string{}
			containerArgs := []string{}
			mountFileCommand := []string{}

			// if container has a command and args, call parseContainerCommandAndReturnArgs
			if len(container.Command) > 0 || len(container.Args) > 0 {
				mountFileCommand, containerCommands, containerArgs, err = parseContainerCommandAndReturnArgs(h.Ctx, h.Config, podUID, podNamespace, container)
				if err != nil {
					HandleErrorAndRemoveData(h, w, "An error occurred during the parse of the container commands and arguments", err, podNamespace, podUID)
					return dockerRunStructs, errors.New("An error occurred during the parse of the container commands and arguments")
				}
				cmd = append(cmd, mountFileCommand...)
			}

			if dockerFlags, ok := podData.Pod.ObjectMeta.Annotations["docker-options.vk.io/flags"]; ok {
				parsedDockerOptions := strings.Split(dockerFlags, " ")
				cmd = append(cmd, parsedDockerOptions...)
			}

			cmd = append(cmd, container.Image)
			cmd = append(cmd, containerCommands...)
			cmd = append(cmd, escapeDockerArgs(containerArgs)...)

			dockerRunStructs = append(dockerRunStructs, DockerRunStruct{
				Name:            containerName,
				Command:         "docker " + strings.Join(cmd, " "),
				IsInitContainer: isInitContainer,
				GpuArgs:         gpuArgs,
				FpgaArgs:        fpgaArgs,
			})
		}
	}

	return dockerRunStructs, nil
}

func (h *SidecarHandler) CreateHandler(w http.ResponseWriter, r *http.Request) {

	log.G(h.Ctx).Info("\u23F3 [CREATE CALL] Received create call from InterLink ")

	start := time.Now().UnixMicro()
	tracer := otel.Tracer("interlink-API")
	_, span := tracer.Start(h.Ctx, "Create", trace.WithAttributes(
		attribute.Int64("start.timestamp", start),
	))

	// create bool variable to set if a new dind container has to be created
	newDindContainerCreated := false

	// get a dind container ID from dind manager of the sidecard handler
	dindContainerID, err := h.DindManager.GetAvailableDind()
	if err != nil {

		log.G(h.Ctx).Info("\u2705 [POD FLOW] No available DIND container found, creating a new one")

		h.DindManager.BuildDindContainers(1)
		dindContainerID, err = h.DindManager.GetAvailableDind()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "During creation of new DIND container, an error occurred during the request of get available DIND container", err, "", "")
			return
		}
		newDindContainerCreated = true
	}

	// remove the dind container from the list of available dind containers
	err = h.DindManager.SetDindUnavailable(dindContainerID)
	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during the removal of the DIND container from the list of available DIND containers", err, "", "")
		return
	}

	if !newDindContainerCreated {
		// create a new dind container in background
		go h.DindManager.BuildDindContainers(1)
	}

	//var execReturn exec.ExecResult
	statusCode := http.StatusOK

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during read of body request for pod creation", err, "", "")
		return
	}

	var data commonIL.RetrievedPodData
	err = json.Unmarshal(bodyBytes, &data)

	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during json unmarshal of data from pod creation request", err, "", "")
		return
	}

	wd, err := os.Getwd()
	if err != nil {
		HandleErrorAndRemoveData(h, w, "Unable to get current working directory", err, "", "")
		return
	}

	log.G(h.Ctx).Info("\u2705 [POD FLOW] Request data unmarshalled successfully and current working directory detected")

	podUID := string(data.Pod.UID)
	podNamespace := string(data.Pod.Namespace)

	podDirectoryPath := filepath.Join(wd, h.Config.DataRootFolder+"/"+podNamespace+"-"+podUID)

	podIpAddress := ""
	annotations := make([]string, 0, len(data.Pod.Annotations))
	for key, value := range data.Pod.Annotations {
		annotations = append(annotations, key+"="+value)

		// if the key is interlink.eu/pod-ip and the value is not empty, set the pod IP to the DIND container
		if key == "interlink.eu/pod-ip" && value != "" {
			podIpAddress = value
		}
	}
	log.G(h.Ctx).Info("\u2705 [POD FLOW] Pod Annotations are: " + strings.Join(annotations, ", "))

	log.G(h.Ctx).Info("\u2705 [POD FLOW] Pod IP Address is: " + podIpAddress)

	// if podIpAddress is != "" then exec the command docker network connect vk0 --ip podIpAddress <container_name>
	if podIpAddress != "" {
		shell := exec.ExecTask{
			Command: "docker",
			Args:    []string{"network", "connect", "vk0", "--ip", podIpAddress, dindContainerID},
			Shell:   true,
		}

		_, err = shell.Execute()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the connection of the DIND container to the vk0 network", err, "", "")
			return
		}

		routeIP := strings.Split(podIpAddress, ".")
		routeIP[3] = "251"
		route := strings.Join(routeIP, ".")

		log.G(h.Ctx).Info("\u2705 [POD FLOW] Route IP is: " + route)

		// inside the dind container, add the route to the pod IP ip route add 10.0.0.0/8  via 10.244.12.251
		shell = exec.ExecTask{
			Command: "docker",
			Args:    []string{"exec", dindContainerID, "ip", "route", "add", "10.0.0.0/8", "via", route},
			Shell:   true,
		}

		_, err = shell.Execute()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the addition of the route to the pod IP", err, "", "")
			return
		}

		// exec the command echo "nameserver 10.96.0.10" > /etc/resolv.conf
		// shell = exec.ExecTask{
		// 	Command: "docker",
		// 	Args:    []string{"exec", dindContainerID, "-u", "0", "sh", "-c", "echo 'nameserver 10.96.0.10' > /etc/resolv.conf"},
		// 	Shell:   true,
		// }

		// log.G(h.Ctx).Info("\u2705 [POD FLOW] Executing command to add nameserver to resolv.conf file")
		// log.G(h.Ctx).Info("\u2705 [POD FLOW] Command: " + "docker " + strings.Join(shell.Args, " "))

		// _, err = shell.Execute()
		// if err != nil {
		// 	HandleErrorAndRemoveData(h, w, "An error occurred during the addition of the nameserver to the resolv.conf file", err, "", "")
		// 	return
		// }

		// log.G(h.Ctx).Info("\u2705 [POD FLOW] Nameserver added to resolv.conf file")

	}

	// if the podDirectoryPath does not exist, create it
	if _, err := os.Stat(podDirectoryPath); os.IsNotExist(err) {
		err = os.MkdirAll(podDirectoryPath, os.ModePerm)
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the creation of the pod directory", err, "", "")
			return
		}
	}

	// call prepareDockerRuns to get the DockerRunStruct array
	dockerRunStructs, err := h.prepareDockerRuns(data, w, podIpAddress)
	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during preparing of docker run commmands", err, "", "")
		return
	}

	log.G(h.Ctx).Info("\u2705 [POD FLOW] Docker run commands prepared successfully")

	// from dockerRunStructs, create two arrays: one for initContainers and one for containers
	var initContainers []DockerRunStruct
	var containers []DockerRunStruct
	//var gpuArgs string

	for _, dockerRunStruct := range dockerRunStructs {
		if dockerRunStruct.IsInitContainer {
			initContainers = append(initContainers, dockerRunStruct)
		} else {
			containers = append(containers, dockerRunStruct)
		}
	}

	// set the podUID to the dind container
	err = h.DindManager.SetPodUIDToDind(dindContainerID, podUID)
	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during the setting of the pod UID to the DIND container", err, "", "")
		return
	}

	// run the docker command to rename the container to the pod UID
	shell := exec.ExecTask{
		Command: "docker",
		Args:    []string{"rename", dindContainerID, string(data.Pod.UID) + "_dind"},
		Shell:   true,
	}

	_, err = shell.Execute()
	if err != nil {
		HandleErrorAndRemoveData(h, w, "An error occurred during the rename of the DIND container", err, "", "")
		return
	}

	createResponse := CreateStruct{PodUID: string(data.Pod.UID), PodJID: dindContainerID}
	createResponseBytes, err := json.Marshal(createResponse)
	if err != nil {
		statusCode = http.StatusInternalServerError
		HandleErrorAndRemoveData(h, w, "An error occurred during the json marshal of the returned JID", err, "", "")
		return
	}

	span.SetAttributes(attribute.String("podUID", string(data.Pod.UID)))
	span.SetAttributes(attribute.String("podJID", dindContainerID))
	span.SetAttributes(attribute.String("podNamespace", string(data.Pod.Namespace)))
	span.SetAttributes(attribute.String("podName", string(data.Pod.Name)))

	w.WriteHeader(statusCode)

	if statusCode != http.StatusOK {
		w.Write([]byte("Some errors occurred while creating containers. Check Docker Sidecar's logs"))
	} else {
		w.Write(createResponseBytes)
	}

	if err != nil {
		span.SetAttributes(attribute.String("error", err.Error()))
	}
	commonIL.SetDurationSpan(start, span, commonIL.WithHTTPReturnCode(statusCode))
	span.End()

	go func() {

		if len(initContainers) > 0 {

			log.G(h.Ctx).Info("\u2705 [POD FLOW] Start creating init containers")

			// Create a list to hold the docker run commands
			var initContainerCommands []string

			// Build the docker run commands for each init container
			for _, initContainer := range initContainers {
				initContainerCommands = append(initContainerCommands, initContainer.Command+"\n")
			}

			// Log the init container commands
			log.G(h.Ctx).Info("\u2705 [POD FLOW] Init containers command list: " + strings.Join(initContainerCommands, ", "))

			// Run init containers sequentially
			for _, initContainer := range initContainers {
				log.G(h.Ctx).Info("\u2705 [POD FLOW] Executing init container: " + initContainer.Name)

				// Execute the docker command for the current init container
				shell := exec.ExecTask{
					Command: "docker",
					Args:    []string{"exec", string(data.Pod.UID) + "_dind", "/bin/sh", "-c", initContainer.Command},
				}

				_, err := shell.Execute()
				if err != nil {
					HandleErrorAndRemoveData(h, w, "An error occurred during the exec of the init container command", err, "", "")
					return
				}

				// Poll the container status until it exits
				for {
					shell = exec.ExecTask{
						Command: "docker",
						Args:    []string{"exec", string(data.Pod.UID) + "_dind", "docker", "inspect", "--format='{{.State.Status}}'", initContainer.Name},
					}

					statusReturn, err := shell.Execute()
					if err != nil {
						HandleErrorAndRemoveData(h, w, "An error occurred during inspect of init container", err, "", "")
						return
					}

					status := strings.Trim(statusReturn.Stdout, "'\n")
					if status == "exited" {
						log.G(h.Ctx).Info("\u2705 [POD FLOW] Init container " + initContainer.Name + " has completed")
						break
					} else {
						time.Sleep(1 * time.Second) // Wait for a second before polling again
					}
				}
			}

			log.G(h.Ctx).Info("\u2705 [POD FLOW] All init containers created and executed successfully")
		}

		// create a file called containers_command.sh and write the containers commands to it, use WriteFile function
		containersCommand := "#!/bin/sh\n"

		// if podIpAddress is != "" , add the echo "nameserver 10.0 " > /etc/resolv.conf command to the containers_command.sh
		if podIpAddress != "" {
			containersCommand += "echo 'nameserver 10.96.0.10' > /etc/resolv.conf" + "\n"
		}

		for _, container := range containers {
			containersCommand += container.Command + "\n"
		}
		err = os.WriteFile(podDirectoryPath+"/containers_command.sh", []byte(containersCommand), 0644)
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the creation of the container commands script.", err, "", "")
			return
		}

		log.G(h.Ctx).Info("\u2705 [POD FLOW] Containers commands written to the script file")

		shell = exec.ExecTask{
			Command: "docker",
			Args:    []string{"exec", string(data.Pod.UID) + "_dind", "/bin/sh", podDirectoryPath + "/containers_command.sh"},
		}

		_, err = shell.Execute()
		if err != nil {
			HandleErrorAndRemoveData(h, w, "An error occurred during the execution of the container command script", err, "", "")
			return
		}

		log.G(h.Ctx).Info("\u2705 [POD FLOW] Containers created successfully")
	}()

}

func HandleErrorAndRemoveData(h *SidecarHandler, w http.ResponseWriter, s string, err error, podNamespace string, podUID string) {
	log.G(h.Ctx).Error(err)
	log.G(h.Ctx).Info("\u274C Error description: " + s)
	w.WriteHeader(http.StatusInternalServerError)
	w.Write([]byte("Some errors occurred while creating container. Check Docker Sidecar's logs"))

	if podNamespace != "" && podUID != "" {
		os.RemoveAll(h.Config.DataRootFolder + podNamespace + "-" + podUID)
	}
	dindSpec := dindmanager.DindSpecs{}
	dindSpec, err = h.DindManager.GetDindFromPodUID(podUID)

	if err != nil {
		log.G(h.Ctx).Error("\u274C [CREATE CALL] Error retrieving DindSpecs, maybe the Dind container has already been deleted")
	} else {
		log.G(h.Ctx).Info("\u2705 [CREATE CALL] Retrieved DindSpecs: " + dindSpec.DindID + " " + dindSpec.PodUID + " " + dindSpec.DindNetworkID + " ")

		// log the retrieved dindSpec
		log.G(h.Ctx).Info("\u2705 [CREATE CALL] Retrieved DindSpecs: " + dindSpec.DindID + " " + dindSpec.PodUID + " " + dindSpec.DindNetworkID + " ")

		cmd := []string{"network", "rm", dindSpec.DindNetworkID}
		shell := exec.ExecTask{
			Command: "docker",
			Args:    cmd,
			Shell:   true,
		}
		execReturn, _ := shell.Execute()
		execReturn.Stdout = strings.ReplaceAll(execReturn.Stdout, "\n", "")
		if execReturn.Stderr != "" {
			log.G(h.Ctx).Error("\u274C [CREATE CALL] Error deleting network " + dindSpec.DindNetworkID)
		} else {
			log.G(h.Ctx).Info("\u2705 [CREATE CALL] Deleted network " + dindSpec.DindNetworkID)
		}
		// set the dind available again
		err = h.DindManager.RemoveDindFromList(dindSpec.PodUID)
		if err != nil {
			log.G(h.Ctx).Error("\u274C [CREATE CALL] Error setting DIND container available")
		}
	}
}
