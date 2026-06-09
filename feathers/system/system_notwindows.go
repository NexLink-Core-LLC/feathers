//go:build !windows

package system

import (
	"context"
	"runtime"

	"github.com/acobaugh/osrelease"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/parsers/kernel"
)

func GetSystemInformation() (*Information, error) {
	k, err := kernel.GetKernelVersion()
	if err != nil {
		return nil, err
	}

	version, info, err := GetDockerInfo(context.Background())
	if err != nil {
		return nil, err
	}

	release, err := osrelease.Read()
	if err != nil {
		return nil, err
	}

	var os string
	if release["PRETTY_NAME"] != "" {
		os = release["PRETTY_NAME"]
	} else if release["NAME"] != "" {
		os = release["NAME"]
	} else {
		os = info.OperatingSystem
	}

	var filesystem string
	for _, v := range info.DriverStatus {
		if v[0] != "Backing Filesystem" {
			continue
		}
		filesystem = v[1]
		break
	}

	return &Information{
		Version: Version,
		Docker: DockerInformation{
			Version: version.Version,
			Cgroups: DockerCgroups{
				Driver:  info.CgroupDriver,
				Version: info.CgroupVersion,
			},
			Containers: DockerContainers{
				Total:   info.Containers,
				Running: info.ContainersRunning,
				Paused:  info.ContainersPaused,
				Stopped: info.ContainersStopped,
			},
			Storage: DockerStorage{
				Driver:     info.Driver,
				Filesystem: filesystem,
			},
			Runc: DockerRunc{
				Version: info.RuncCommit.ID,
			},
		},
		System: System{
			Architecture:  runtime.GOARCH,
			CPUThreads:    runtime.NumCPU(),
			MemoryBytes:   info.MemTotal,
			KernelVersion: k.String(),
			OS:            os,
			OSType:        runtime.GOOS,
		},
	}, nil
}

func GetDockerInfo(ctx context.Context) (types.Version, system.Info, error) {
	// TODO: find a way to re-use the client from the docker environment.
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return types.Version{}, system.Info{}, err
	}
	defer c.Close()

	dockerVersion, err := c.ServerVersion(ctx)
	if err != nil {
		return types.Version{}, system.Info{}, err
	}

	dockerInfo, err := c.Info(ctx)
	if err != nil {
		return types.Version{}, system.Info{}, err
	}

	return dockerVersion, dockerInfo, nil
}
