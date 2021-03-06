package services

import (
	"github.com/Sirupsen/logrus"
	"linkernetworks.com/linker_common_lib/entity"
	"linkernetworks.com/linker_dcos_deploy/command"
	"strings"
	"sync"
)

type DockerComposeService struct {
	serviceName string
}

var (
	dockerComposeService *DockerComposeService = nil
	onceDockerCompose    sync.Once
)

func GetDockerComposeService() *DockerComposeService {
	onceDockerCompose.Do(func() {
		logrus.Debugf("Once called from DockerComposeService ......................................")
		dockerComposeService = &DockerComposeService{"DockerComposeService"}
	})
	return dockerComposeService

}

func (p *DockerComposeService) Add(username, clusterName string, addServer []entity.Server, curScale int, swarmMaster string) error {
	storagePath := DOCKERMACHINE_STORAGEPATH_PREFIX + username + "/" + clusterName + ""
	nodeList := []string{}
	for _, tmpHost := range addServer {
		strTmp := strings.Replace(tmpHost.Hostname, ".", "_", -1)
		strTmp = strings.Replace(strTmp, "-", "_", -1)
		tmpStr := strTmp + "=" + tmpHost.IpAddress
		nodeList = append(nodeList, tmpStr)
	}
	return command.AddSlaveToCluster(username, clusterName, swarmMaster, storagePath, nodeList, curScale)
}

func (p *DockerComposeService) Remove(username, clusterName, serverName string) error {
	return command.RemoveSlaveFromCluster(username, clusterName, serverName)
}

func (p *DockerComposeService) Create(username, clusterName string, allServers []entity.Server, scale int) error {
	nodeList := []string{}
	masterList := []string{}
	swarmMaster := ""
	storagePath := DOCKERMACHINE_STORAGEPATH_PREFIX + username + "/" + clusterName + ""
	for _, tmpHost := range allServers {
		if tmpHost.IsSwarmMaster {
			swarmMaster = tmpHost.Hostname
		}
		if tmpHost.IsMaster {
			masterList = append(masterList, tmpHost.PrivateIpAddress)
		}
		// if tmpHost.IsSlave {
		strTmp := strings.Replace(tmpHost.Hostname, ".", "_", -1)
		strTmp = strings.Replace(strTmp, "-", "_", -1)
		tmpStr := strTmp + "=" + tmpHost.IpAddress
		nodeList = append(nodeList, tmpStr)
		// }
	}
	return command.InstallCluster(username, clusterName, swarmMaster, storagePath, masterList, nodeList, scale)
}
