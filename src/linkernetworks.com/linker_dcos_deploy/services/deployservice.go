package services

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Sirupsen/logrus"
	"io/ioutil"
	"linkernetworks.com/linker_common_lib/entity"
	"linkernetworks.com/linker_dcos_deploy/command"
	"sync"
	"time"
)

const (
	DEPLOY_STATUS_TERMINATED = "TERMINATED"
	DEPLOY_STATUS_DEPLOYED   = "RUNNING"
	DEPLOY_STATUS_DEPLOYING  = "DEPLOYING"
	DEPLOY_STATUS_FAILED     = "FAILED"

	DEPLOY_ERROR_CREATE string = "E60000"
	DEPLOY_ERROR_SCALE  string = "E60001"
	DEPLOY_ERROR_DELETE string = "E60002"
	DEPLOY_ERROR_QUERY  string = "E60003"

	DEPLOY_ERROR_CHANGE_HOST       string = "E60010"
	DEPLOY_ERROR_CHANGE_NAMESERVER string = "E60011"
	DEPLOY_ERROR_CHANGE_DNSCONFIG  string = "E60012"
	DEPLOY_ERROR_COPY_CONFIG_FILE  string = "E60013"
)

var (
	deployService *DeployService = nil
	onceDeploy    sync.Once
	RetryTime     = 100
)

type DeployService struct {
	serviceName string
}

func GetDeployService() *DeployService {
	onceDeploy.Do(func() {
		logrus.Debugf("Once called from DeployService ......................................")
		deployService = &DeployService{"DeployService"}
	})
	return deployService
}

func (p *DeployService) CreateCluster(request entity.Request) (
	servers []entity.Server, errorCode string, err error) {
	logrus.Infof("start to Deploy Docker Cluster...")
	//Call the Docker Machines to create machines, change /etc/hosts and Replace PubKey File
	servers, swarmServers, mgmtServers, dnsServers, _, err := dockerMachineCreateCluster(request)

	if err != nil {
		logrus.Errorf("Call docker-machine to create cluster failed , err is %v", err)
		return
	}

	storagePath := DOCKERMACHINE_STORAGEPATH_PREFIX + request.UserName + "/" + request.ClusterName

	//TODO prepare ".env" file for new Cluster, need to understand whether 'slave on master'
	//call docker-compose to deploy the management clusters and slave clusters
	clusterSlaveSize := request.ClusterNumber - 1
	if request.IsLinkerMgmt == false {
		clusterSlaveSize = request.ClusterNumber - 4
	}

	err = dockerComposeCreateCluster(request.UserName, request.ClusterName, swarmServers, clusterSlaveSize)

	if err != nil {
		logrus.Errorf("Call docker-compose to create cluster failed , err is %v", err)
		return
	}

	//get the first ip of mgmtServer as marathon Ip
	marathonEndpoint := fmt.Sprintf("%s:8080", mgmtServers[0].PrivateIpAddress)

	//prepare the dns config and copy to all managements nodes
	err = changeDnsConfig(mgmtServers)
	if err != nil {
		errorCode = DEPLOY_ERROR_CHANGE_DNSCONFIG
		return
	}

	//copy the config file to target dns server
	for _, server := range dnsServers {
		_, _, err = command.ExecCommandOnMachine(server.Hostname, "sudo mkdir -p /linker/config", storagePath)
		if err != nil {
			errorCode = DEPLOY_ERROR_COPY_CONFIG_FILE
			logrus.Errorf("mkdir /linker/config failed when copy dns config file", err)
			return
		}
		_, _, err = command.ScpToMachine(server.Hostname, "/linker/config/config.json", "/linker/config/config.json", storagePath)
		if err != nil {
			errorCode = DEPLOY_ERROR_COPY_CONFIG_FILE
			logrus.Errorf("copy dns config file to target server %s fail, err is %v", server.Hostname, err)
			return
		}
	}

	//Change "/etc/resolve.conf" if Linker Management Cluster
	err = changeNameserver(swarmServers, dnsServers, storagePath, request.IsLinkerMgmt)
	if err != nil {
		errorCode = DEPLOY_ERROR_CHANGE_NAMESERVER
		return
	}

	//call the marathon to deploy the mesos-DNS and marathon-lb for Linker-Management Cluster
	payload := prepareDNSandLbJson(request.UserName, request.ClusterName, dnsServers[0], request.IsLinkerMgmt)
	deploymentId, _ := GetMarathonService().CreateGroup(payload, marathonEndpoint)
	if waitForMarathon(deploymentId, marathonEndpoint) {
		errorCode = DEPLOY_ERROR_CREATE
		logrus.Errorf("deploy the dns and lb fail on, err is %v", err)
		return
	}

	//Start create Linker Components
	if request.IsLinkerMgmt {
		//copy the key for mongo db
		for _, server := range mgmtServers {
			_, _, err = command.ExecCommandOnMachine(server.Hostname, "sudo mkdir -p /linker/key", storagePath)
			if err != nil {
				errorCode = DEPLOY_ERROR_COPY_CONFIG_FILE
				logrus.Errorf("mkdir /linker/config failed when copy dns config file", err)
				return
			}
			_, _, err = command.ScpToMachine(server.Hostname, "/linker/key/mongodb-keyfile", "/linker/key/mongodb-keyfile", storagePath)
			if err != nil {
				errorCode = DEPLOY_ERROR_COPY_CONFIG_FILE
				logrus.Errorf("copy mongodb key file to target server %s fail, err is %v", server.Hostname, err)
				return
			}
		}

		//call the marathon to deploy the linker service containers for Linker-Management Cluster
		payload = prepareLinkerComponents(mgmtServers)
		deploymentId, _ = GetMarathonService().CreateGroup(payload, marathonEndpoint)
		if waitForMarathon(deploymentId, marathonEndpoint) {
			errorCode = DEPLOY_ERROR_CREATE
			logrus.Errorf("deploy the linker components fail , err is %v", err)
			return
		}
	}

	//call the marathon to deploy the mesos-UI for Linker-Management Cluster and User-Management Cluster
	payload = prepareMesosUI(mgmtServers, dnsServers[0], request.IsLinkerMgmt)
	deploymentId, _ = GetMarathonService().CreateGroup(payload, marathonEndpoint)
	if waitForMarathon(deploymentId, marathonEndpoint) {
		errorCode = DEPLOY_ERROR_CREATE
		logrus.Errorf("deploy the linker components fail , err is %v", err)
		return
	}
	return
}

func prepareLinkerComponents(mgmtServers []entity.Server) (payload []byte) {
	payload, err := ioutil.ReadFile("/linker/marathon/marathon-linkercomponents.json")

	if err != nil {
		logrus.Errorf("read linkercomponents.json failed, error is %v", err)
		return
	}

	var serviceGroup *entity.ServiceGroup
	err = json.Unmarshal(payload, &serviceGroup)
	if err != nil {
		logrus.Errorf("Unmarshal linkercomponents.json failed, error is %v", err)
		return
	}

	mongoserverlist := ""
	var commandTextBuffer bytes.Buffer
	for index, server := range mgmtServers {
		commandTextBuffer.WriteString(server.PrivateIpAddress)
		if index != len(mgmtServers)-1 {
			commandTextBuffer.WriteString(",")
		}
	}

	mongoserverlist = commandTextBuffer.String()

	for _, group := range serviceGroup.Groups {
		// There is no case for group embeded group.
		for _, app := range group.Apps {
			app.Env["MONGODB_NODES"] = mongoserverlist
		}
	}

	payload, err = json.Marshal(*serviceGroup)
	if err != nil {
		logrus.Errorf("Marshal linkercomponents err is %v", err)
		return
	}

	return payload
}

func prepareMesosUI(mgmtServers []entity.Server, dnsServer entity.Server, isLinkerMgmt bool) (payload []byte) {
	payload, err := ioutil.ReadFile("/linker/marathon/marathon-dashboard.json")

	constraint := []string{"hostname", "CLUSTER", dnsServer.IpAddress}

	if err != nil {
		logrus.Errorf("read marathon-dashboard.json failed, error is %v", err)
		return
	}

	var serviceGroup *entity.ServiceGroup
	err = json.Unmarshal(payload, &serviceGroup)
	if err != nil {
		logrus.Errorf("Unmarshal marathon-dashboard.json failed, error is %v", err)
		return
	}

	mongoserverlist := ""
	var commandTextBuffer bytes.Buffer
	for index, server := range mgmtServers {
		commandTextBuffer.WriteString(server.PrivateIpAddress)
		if index != len(mgmtServers)-1 {
			commandTextBuffer.WriteString(",")
		}
	}

	mongoserverlist = commandTextBuffer.String()

	for _, group := range serviceGroup.Groups {
		// There is no case for group embeded group.
		for _, app := range group.Apps {
			app.Env["marathon-dashboard"] = mongoserverlist
			if !isLinkerMgmt {
				app.Constraints = [][]string{}
				app.Constraints = append(app.Constraints, constraint)
			}
		}
	}

	payload, err = json.Marshal(*serviceGroup)
	if err != nil {
		logrus.Errorf("Marshal marathon-dashboard err is %v", err)
		return
	}

	return payload
}

func prepareDNSandLbJson(username, clustername string, dnsServer entity.Server, isLinkerMgmt bool) (payload []byte) {
	payload, err := ioutil.ReadFile("/linker/marathon/marathon-dnslb.json")

	constraint := []string{"hostname", "CLUSTER", dnsServer.IpAddress}

	if err != nil {
		logrus.Errorf("read mesos dns and marathon lb.json failed, error is %v", err)
		return
	}

	if !isLinkerMgmt {
		var serviceGroup *entity.ServiceGroup
		err = json.Unmarshal(payload, &serviceGroup)
		if err != nil {
			logrus.Errorf("Unmarshal mesos dns and marathon lb.json failed, error is %v", err)
			return
		}
		serviceGroup.Id = fmt.Sprintf("/%s-%s-dns", username, clustername)

		for _, group := range serviceGroup.Groups {
			// Add constraints
			// There is no case for group embeded group.
			for _, app := range group.Apps {
				app.Constraints = [][]string{}
				app.Constraints = append(app.Constraints, constraint)
			}
		}

		payload, err = json.Marshal(*serviceGroup)
		if err != nil {
			logrus.Errorf("Marshal mesos dns and marathon lb err is %v", err)
			return
		}
	}
	return
}

func dockerComposeCreateCluster(username, clustername string, swarmServers []entity.Server, scale int) (err error) {
	err = GetDockerComposeService().Create(username, clustername, swarmServers, scale)
	return err
}

func waitForMarathon(deploymentId, marathonEndpoint string) (flag bool) {
	flag = false
	logrus.Debugf("check status with deploymentId [%v]", deploymentId)
	for i := 0; i < RetryTime; i++ {
		// get lock by service group instance id
		flag, _ = GetMarathonService().IsDeploymentDone(deploymentId, marathonEndpoint)
		if flag {
			logrus.Debugf("deployment Finished with  deploymentId[%v]", deploymentId)
			return flag
		} else {
			time.Sleep(30000 * time.Millisecond)
		}
	}
	return flag
}

func changeDnsConfig(mgmtServers []entity.Server) (err error) {
	dat, err := ioutil.ReadFile("/linker/config/dns-config.json")
	if err != nil {
		logrus.Errorf("read dns-config.json failed, error is %v", err)
		return
	}

	var dnsconfig *entity.DnsConfig
	err = json.Unmarshal(dat, &dnsconfig)

	if err != nil {
		logrus.Errorf("Unmarshal DnsConfig err is %v", err)
		return
	}

	//make zookeeper string
	var commandZkBuffer bytes.Buffer
	masterGroup := []string{}
	commandZkBuffer.WriteString("zk://")
	for i, server := range mgmtServers {
		commandZkBuffer.WriteString(server.IpAddress)
		commandZkBuffer.WriteString(":2181")
		if i < len(mgmtServers) {
			commandZkBuffer.WriteString(",")
		}
		var commandMasterBuffer bytes.Buffer
		commandMasterBuffer.WriteString(server.IpAddress)
		commandMasterBuffer.WriteString(":5050")
		masterGroup = append(masterGroup, commandMasterBuffer.String())
	}
	commandZkBuffer.WriteString("/mesos")

	//make zookeeper
	dnsconfig.Zookeeper = commandZkBuffer.String()

	//make masters
	dnsconfig.Masters = append(masterGroup)

	//write back to file
	jsonresult, err := json.Marshal(*dnsconfig)
	if err != nil {
		logrus.Errorf("Marshal DnsConfig err is %v", err)
		return
	}

	err = ioutil.WriteFile("/linker/config/config.json", jsonresult, 0666)
	if err != nil {
		logrus.Errorf("write config.json failed, error is %v", err)
		return
	}
	return
}

func changeNameserver(servers, dnsServers []entity.Server, storagePath string, isLinkerMgmt bool) (err error) {
	if isLinkerMgmt {
		for _, server := range servers {
			commandStr := fmt.Sprintf("sudo sed -i '1s/^/nameserver %s\n /' /etc/resolv.conf", server.IpAddress)
			_, _, err = command.ExecCommandOnMachine(server.Hostname, commandStr, storagePath)
			if err != nil {
				logrus.Errorf("change name server failed for server [%v], error is %v", server.IpAddress, err)
				return
			}
		}
	} else {
		commandStr := fmt.Sprintf("sudo sed -i '1s/^/nameserver %s\n /' /etc/resolv.conf", dnsServers[0].IpAddress)
		for _, server := range servers {
			_, _, err = command.ExecCommandOnMachine(server.Hostname, commandStr, storagePath)
			if err != nil {
				logrus.Errorf("change name server failed for server [%v], error is %v", server.IpAddress, err)
				return
			}
		}
	}
	return
}

func dockerMachineCreateCluster(request entity.Request) (servers, swarmServers, mgmtServers, dnsServers []entity.Server, errorCode string, err error) {
	isLinkerMgmt := request.IsLinkerMgmt
	requestIdLabel := entity.Label{Key: "requestId", Value: request.RequestId}
	masterLabel := entity.Label{Key: "master", Value: "true"}
	slaveLabel := entity.Label{Key: "slave", Value: "true"}

	//create Server and install consule
	labels := []entity.Label{}
	labels = append(labels, requestIdLabel)
	consuleServer, _, err := GetDockerMachineService().Create(request.UserName, request.ClusterName, false, false, "", request.ProviderInfo, labels)
	_, _, err = command.BootUpConsul(consuleServer.Hostname, consuleServer.StoragePath)
	if err != nil {
		err = errors.New("Bootup Consul server error!")
		return
	}
	servers = append(servers, consuleServer)

	if isLinkerMgmt {
		labels := []entity.Label{}
		labels = append(labels, masterLabel)
		labels = append(labels, slaveLabel)
		labels = append(labels, requestIdLabel)
		//create Server, install Swarm Master and Label as Mgmt and Slave Node
		swarmMasterServer, _, _ := GetDockerMachineService().Create(request.UserName, request.ClusterName, true, true, consuleServer.Hostname, request.ProviderInfo, labels)
		swarmMasterServer.IsMaster = true
		swarmMasterServer.IsSlave = true
		swarmMasterServer.IsSwarmMaster = true
		swarmServers = append(swarmServers, swarmMasterServer)
		mgmtServers = append(mgmtServers, swarmMasterServer)
		dnsServers = append(dnsServers, swarmMasterServer)
		servers = append(servers, swarmMasterServer)

		//create Server, install Swarm Slave and Label as Mgmt and Slave Node
		for i := 0; i < request.ClusterNumber-2; i++ {
			server, _, _ := GetDockerMachineService().Create(request.UserName, request.ClusterName, true, false, consuleServer.Hostname, request.ProviderInfo, labels)
			server.IsMaster = true
			server.IsSlave = true
			swarmServers = append(swarmServers, server)
			mgmtServers = append(mgmtServers, server)
			dnsServers = append(dnsServers, swarmMasterServer)
			servers = append(servers, server)
		}
	} else {
		labels := []entity.Label{}
		labels = append(labels, masterLabel)
		labels = append(labels, requestIdLabel)
		//create Server, install Swarm Master and Label as MgmtOnly Node
		swarmMasterServer, _, _ := GetDockerMachineService().Create(request.UserName, request.ClusterName, true, true, consuleServer.Hostname, request.ProviderInfo, labels)
		swarmMasterServer.IsMaster = true
		swarmMasterServer.IsSwarmMaster = true
		swarmServers = append(swarmServers, swarmMasterServer)
		mgmtServers = append(mgmtServers, swarmMasterServer)
		servers = append(servers, swarmMasterServer)

		//create Server, install Swarm Slave and Label as Mgmt Node
		for i := 0; i < 2; i++ {
			server, _, _ := GetDockerMachineService().Create(request.UserName, request.ClusterName, true, false, consuleServer.Hostname, request.ProviderInfo, labels)
			server.IsMaster = true
			swarmServers = append(swarmServers, server)
			mgmtServers = append(mgmtServers, server)
			servers = append(servers, server)
		}

		slavelabels := []entity.Label{}
		slavelabels = append(slavelabels, slaveLabel)
		slavelabels = append(labels, requestIdLabel)
		//create Server, install Swarm Slave and Label as Slave Node
		for i := 0; i < request.ClusterNumber-4; i++ {
			server, _, _ := GetDockerMachineService().Create(request.UserName, request.ClusterName, true, false, consuleServer.Hostname, request.ProviderInfo, slavelabels)
			server.IsSlave = true
			swarmServers = append(swarmServers, server)
			servers = append(servers, server)
			//choose the first slave server as shared server
			if i == 0 {
				dnsServers = append(dnsServers, server)
			}
		}
	}
	return
}

func (p *DeployService) DeleteCluster(x_auth_token string) (
	errorCode string, err error) {
	logrus.Infof("start to Delete Docker Cluster...")
	//get the cluster name and user info to call docker machine to delete all the vms

	//Notify Cluster Service if Delete User Cluster

	return
}
func (p *DeployService) DeleteNode(x_auth_token string) (
	errorCode string, err error) {
	logrus.Infof("start to Delete Docker Machine...")
	//remove the ip from env file

	//call docker machine to terminate the vm

	//notify the Cluster Service

	return
}

func (p *DeployService) CreateNode(x_auth_token string) (
	errorCode string, err error) {
	logrus.Infof("start to Scale Docker Machine...")
	//Call the Docker Machines to create machines and Replace PubKey File

	//Change the "/etc/hosts" and "/etc/resolve.conf" of server

	//prepare ".env" file for new Node

	//call docker-compose to deploy the slave Node

	//notify the Cluster Service

	return
}
