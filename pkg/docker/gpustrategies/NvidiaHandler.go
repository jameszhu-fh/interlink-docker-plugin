package gpustrategies

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"strconv"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/containerd/log"

	//"github.com/containerd/containerd/log"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"sync"
)

type GPUSpecs struct {
	Name        string
	UUID        string
	Type        string
	ContainerID string
	Available   bool
	Index       int
}

type GPUManager struct {
	GPUSpecsList  []GPUSpecs
	GPUSpecsMutex sync.Mutex // Mutex to make GPUSpecsList access atomic
	Vendor        string
	Ctx           context.Context
}

type GPUManagerInterface interface {
	Init() error
	Shutdown() error
	GetGPUSpecsList() []GPUSpecs
	Dump() error
	Discover() error
	Check() error
	GetAvailableGPUs(numGPUs int) ([]GPUSpecs, error)
	Assign(UUID string, containerID string) error
	Release(UUID string) error
	GetAndAssignAvailableGPUs(numGPUs int, containerID string) ([]GPUSpecs, error)
}

func (a *GPUManager) Init() error {

	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("Unable to initialize NVML")
	}

	return nil
}

// Discover implements the Discover function of the GPUManager interface
func (a *GPUManager) Discover() error {

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("Unable to get device count: %v", nvml.ErrorString(ret))
	}

	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return fmt.Errorf("Unable to get device at index %d: %v", i, nvml.ErrorString(ret))
		}

		uuid, ret := device.GetUUID()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("Unable to get uuid of device at index %d: %v", i, nvml.ErrorString(ret))
		}

		name, ret := device.GetName()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("Unable to get name of device at index %d: %v", i, nvml.ErrorString(ret))
		}

		index, ret := device.GetIndex()
		if ret != nvml.SUCCESS {
			return fmt.Errorf("Unable to get index of device at index %d: %v", i, nvml.ErrorString(ret))
		}

		// Add the GPU to the GPUSpecsList
		a.GPUSpecsList = append(a.GPUSpecsList, GPUSpecs{Name: name, UUID: uuid, Type: "NVIDIA", ContainerID: "", Available: true, Index: index})
	}

	// print the GPUSpecsList if the length is greater than 0
	if len(a.GPUSpecsList) > 0 {
		log.G(a.Ctx).Info("\u2705 Discovered GPUs:")
		for _, gpuSpec := range a.GPUSpecsList {
			log.G(a.Ctx).Info(fmt.Sprintf("\u2705 Name: %s, UUID: %s, Type: %s, Available: %t, Index: %d", gpuSpec.Name, gpuSpec.UUID, gpuSpec.Type, gpuSpec.Available, gpuSpec.Index))
		}
	} else {
		log.G(a.Ctx).Info(" \u2705 No GPUs discovered")
	}

	return nil
}

func (a *GPUManager) Check() error {

	//cli, err := client.NewEnvClient()
	cli, err := client.NewClientWithOpts(client.WithVersion("1.44"))
	if err != nil {
		return fmt.Errorf("unable to create a new Docker client: %v", err)
	}

	containers, err := cli.ContainerList(context.Background(), container.ListOptions{All: false}) // With All set to false I get only the running containers, if I set All to true I get all the containers (running and stopped)
	if err != nil {
		return fmt.Errorf("unable to list containers: %v", err)
	}

	for _, container := range containers {
		containerInfo, err := cli.ContainerInspect(context.Background(), container.ID)
		if err != nil {
			return fmt.Errorf("unable to inspect container: %v", err)
		}

		for _, env := range containerInfo.Config.Env {
			if strings.Contains(env, "NVIDIA_VISIBLE_DEVICES=") {

				indexOfEqualSign := strings.Index(env, "=")
				gpuIDs := env[indexOfEqualSign+1:]
				gpuIDsSplitted := strings.Split(gpuIDs, ",")

				for _, gpuID := range gpuIDsSplitted {
					gpuIndex, err := strconv.Atoi(gpuID)
					if err != nil {
						fmt.Errorf("unable to convert GPU ID to int: %v", err)
						continue
					}
					for i := range a.GPUSpecsList {
						if a.GPUSpecsList[i].Index == gpuIndex {
							a.GPUSpecsList[i].ContainerID = containerInfo.ID
							a.GPUSpecsList[i].Available = false
						}
					}
				}
			}
		}
	}

	// print the GPUSpecsList that are not available
	for _, gpuSpec := range a.GPUSpecsList {
		if !gpuSpec.Available {
			log.G(a.Ctx).Info(fmt.Sprintf("\u274C GPU with UUID %s is not available. It is in use by container %s", gpuSpec.UUID, gpuSpec.ContainerID))
		} else {
			log.G(a.Ctx).Info(fmt.Sprintf("\u2705 GPU with UUID %s is available", gpuSpec.UUID))
		}
	}

	return nil
}

func (a *GPUManager) Shutdown() error {

	ret := nvml.Shutdown()
	if ret != nvml.SUCCESS {
		return fmt.Errorf("Unable to shutdown NVML: %v", nvml.ErrorString(ret))
	}

	return nil
}

func (a *GPUManager) GetGPUSpecsList() []GPUSpecs {
	return a.GPUSpecsList
}

func (a *GPUManager) Assign(UUID string, containerID string) error {

	for i := range a.GPUSpecsList {
		if a.GPUSpecsList[i].UUID == UUID {

			if a.GPUSpecsList[i].Available == false {
				return fmt.Errorf("GPU with UUID %s is already in use by container %s", UUID, a.GPUSpecsList[i].ContainerID)
			}

			a.GPUSpecsList[i].ContainerID = containerID
			a.GPUSpecsList[i].Available = false
			break
		}
	}
	return nil

}

func (a *GPUManager) Release(containerID string) error {

	a.GPUSpecsMutex.Lock()
	defer a.GPUSpecsMutex.Unlock()

	for i := range a.GPUSpecsList {
		if a.GPUSpecsList[i].ContainerID == containerID {

			if a.GPUSpecsList[i].Available == true {
				continue
			}

			a.GPUSpecsList[i].ContainerID = ""
			a.GPUSpecsList[i].Available = true
		}
	}

	return nil
}

func (a *GPUManager) GetAvailableGPUs(numGPUs int) ([]GPUSpecs, error) {

	var availableGPUs []GPUSpecs
	for _, gpuSpec := range a.GPUSpecsList {
		if gpuSpec.Available == true {
			availableGPUs = append(availableGPUs, gpuSpec)
			if len(availableGPUs) == numGPUs {
				return availableGPUs, nil
			}
		}
	}
	return nil, fmt.Errorf("Not enough available GPUs. Requested: %d, Available: %d", numGPUs, len(availableGPUs))
}

func (a *GPUManager) GetAndAssignAvailableGPUs(numGPUs int, containerID string) ([]GPUSpecs, error) {

	a.GPUSpecsMutex.Lock()
	defer a.GPUSpecsMutex.Unlock()

	gpuSpecs, err := a.GetAvailableGPUs(numGPUs)
	if err != nil {
		return nil, err
	}

	for _, gpuSpec := range gpuSpecs {
		err = a.Assign(gpuSpec.UUID, containerID)
		if err != nil {
			return nil, err
		}
	}

	return gpuSpecs, nil
}

// dump the GPUSpecsList into a JSON file
func (a *GPUManager) Dump() error {

	// Convert the array to JSON format
	jsonData, err := json.MarshalIndent(a.GPUSpecsList, "", "  ")
	if err != nil {
		return fmt.Errorf("Error marshalling JSON: %v", err)
	}

	// Write JSON data to a file
	err = ioutil.WriteFile("gpu_specs.json", jsonData, 0644)
	if err != nil {
		return fmt.Errorf("Error writing to file: %v", err)
	}

	return nil
}
