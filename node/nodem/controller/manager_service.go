// Copyright (C) 2014-2018 Goodrain Co., Ltd.
// RAINBOND, Application Management Platform

// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version. For any non-GPL usage of Rainbond,
// one or multiple Commercial Licenses authorized by Goodrain Co., Ltd.
// must be obtained first.

// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.

// You should have received a copy of the GNU General Public License
// along with this program. If not, see <http://www.gnu.org/licenses/>.

package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/clientv3"
	"github.com/goodrain/rainbond/cmd/node/option"
	"github.com/goodrain/rainbond/node/nodem/client"
	"github.com/goodrain/rainbond/node/nodem/healthy"
	"github.com/goodrain/rainbond/node/nodem/service"
)

var (
	ArgsReg = regexp.MustCompile(`\$\{(\w+)\|{0,1}(.{0,1})\}`)
)

//ManagerService manager service
type ManagerService struct {
	node                 client.HostNode
	ctx                  context.Context
	cancel               context.CancelFunc
	syncCtx              context.Context
	syncCancel           context.CancelFunc
	conf                 *option.Conf
	ctr                  Controller
	cluster              client.ClusterClient
	healthyManager       healthy.Manager
	services             *[]*service.Service
	allservice           *[]*service.Service
	etcdcli              *clientv3.Client
	autoStatusController map[string]statusController
	lock                 sync.Mutex
}

//GetAllService get all service
func (m *ManagerService) GetAllService() (*[]*service.Service, error) {
	return m.allservice, nil
}

//Start  start and monitor all service
func (m *ManagerService) Start(node client.HostNode) error {
	logrus.Info("Starting node controller manager.")
	m.loadServiceConfig()
	m.node = node
	if m.conf.EnableInitStart {
		return m.ctr.InitStart(*m.services)
	}
	return nil
}

func (m *ManagerService) loadServiceConfig() {
	*m.allservice = service.LoadServicesFromLocal(m.conf.ServiceListFile)
	var controllerServices []*service.Service
	for _, s := range *m.allservice {
		if !s.OnlyHealthCheck && !s.Disable {
			controllerServices = append(controllerServices, s)
		}
	}
	*m.services = controllerServices
}

//Stop stop manager
func (m *ManagerService) Stop() error {
	m.cancel()
	return nil
}

//Online start all service of on the node
func (m *ManagerService) Online() error {
	logrus.Info("Doing node online by node controller manager")
	// registry local services endpoint into cluster manager
	hostIP := m.cluster.GetOptions().HostIP
	for _, s := range *m.services {
		if s.OnlyHealthCheck || s.Disable {
			continue
		}
		logrus.Debug("Parse endpoints for service: ", s.Name)
		for _, end := range s.Endpoints {
			logrus.Debug("Discovery endpoints: ", end.Name)
			endpoint := toEndpoint(end, hostIP)
			oldEndpoints := m.cluster.GetEndpoints(end.Name)
			if exist := isExistEndpoint(oldEndpoints, endpoint); !exist {
				oldEndpoints = append(oldEndpoints, endpoint)
				m.cluster.SetEndpoints(end.Name, oldEndpoints)
			}
		}
	}
	if ok := m.ctr.CheckBeforeStart(); !ok {
		return nil
	}

	err := m.WriteServices()
	if err != nil {
		return err
	}

	// start all by systemctl start multi-user.target
	m.ctr.StartList(*m.services)
	m.SyncServiceStatusController()

	return nil
}

//Offline stop all service of on the node
func (m *ManagerService) Offline() error {
	logrus.Info("Doing node offline by node controller manager")
	// Anti-registry local services endpoint from cluster manager
	HostIP := m.cluster.GetOptions().HostIP
	services, _ := m.GetAllService()
	for _, s := range *services {
		for _, end := range s.Endpoints {
			logrus.Debug("Anti-registry endpoint: ", end.Name)
			endpoint := toEndpoint(end, HostIP)
			oldEndpoints := m.cluster.GetEndpoints(end.Name)
			if exist := isExistEndpoint(oldEndpoints, endpoint); exist {
				m.cluster.SetEndpoints(end.Name, rmEndpointFrom(oldEndpoints, endpoint))
			}
		}
	}

	m.StopSyncService()

	if err := m.ctr.StopList(*m.services); err != nil {
		return err
	}

	return nil
}

//SyncServiceStatusController synchronize all service status to as we expect
func (m *ManagerService) SyncServiceStatusController() {
	m.lock.Lock()
	defer m.lock.Unlock()
	if m.autoStatusController != nil && len(m.autoStatusController) > 0 {
		for _, v := range m.autoStatusController {
			v.Stop()
		}
	}
	m.autoStatusController = make(map[string]statusController, len(*m.services))
	for _, s := range *m.services {
		ctx, cancel := context.WithCancel(context.Background())
		serviceStatusController := statusController{
			ctx:            ctx,
			cancel:         cancel,
			serviceName:    s.Name,
			healthyManager: m.healthyManager,
			watcher:        m.healthyManager.WatchServiceHealthy(s.Name),
			unhealthHandle: func(event *service.HealthStatus, w healthy.Watcher) {
				// disable check healthy status of the service
				m.healthyManager.DisableWatcher(event.Name, w.GetID())
				m.ctr.RestartService(event.Name)
				if !m.WaitStart(event.Name, time.Minute) {
					logrus.Errorf("Timeout restart service: %s", event.Name)
				}
				// start check healthy status of the service
				m.healthyManager.EnableWatcher(event.Name, w.GetID())
			},
		}
		m.autoStatusController[s.Name] = serviceStatusController
		go serviceStatusController.Run()
	}
}

type statusController struct {
	watcher        healthy.Watcher
	ctx            context.Context
	cancel         context.CancelFunc
	serviceName    string
	unhealthHandle func(event *service.HealthStatus, w healthy.Watcher)
	healthyManager healthy.Manager
}

func (s *statusController) Run() {
	s.healthyManager.EnableWatcher(s.serviceName, s.watcher.GetID())
	defer s.watcher.Close()
	defer s.healthyManager.DisableWatcher(s.serviceName, s.watcher.GetID())
	for {
		select {
		case event := <-s.watcher.Watch():
			switch event.Status {
			case service.Stat_healthy:
				logrus.Debugf("is [%s] of service %s.", event.Status, event.Name)
			case service.Stat_unhealthy:
				logrus.Debugf("is [%s] of service %s %d times.", event.Status, event.Name, event.ErrorNumber)
				if event.ErrorNumber > 3 {
					logrus.Infof("is [%s] of service %s %d times and restart it.", event.Status, event.Name, event.ErrorNumber)
					s.unhealthHandle(event, s.watcher)
				}
			case service.Stat_death:
				logrus.Infof("is [%s] of service %s %d times and start it.", event.Status, event.Name, event.ErrorNumber)
				s.unhealthHandle(event, s.watcher)
			}
		case <-s.ctx.Done():
			return
		}
	}
}
func (s *statusController) Stop() {
	s.cancel()
}

func (m *ManagerService) StopSyncService() {
	if m.syncCtx != nil {
		m.syncCancel()
	}
}

func (m *ManagerService) WaitStart(name string, duration time.Duration) bool {
	max := time.Now().Add(duration)
	t := time.Tick(time.Second * 3)
	for {
		if time.Now().After(max) {
			return false
		}
		status, err := m.healthyManager.GetCurrentServiceHealthy(name)
		if err != nil {
			logrus.Errorf("Can not get %s service current status: %v", name, err)
			<-t
			continue
		}
		logrus.Debugf("Check service %s current status: %s", name, status.Status)
		if status.Status == service.Stat_healthy {
			return true
		}
		<-t
	}
}

/*
1. reload services info from local file system
2. regenerate systemd config file and restart with config changes
3. start all newly added services
*/
func (m *ManagerService) ReLoadServices() error {
	logrus.Info("start reload service configs")
	services := service.LoadServicesFromLocal(m.conf.ServiceListFile)
	var controllerServices []*service.Service
	var restartCount int
	for _, ne := range services {
		if ne.OnlyHealthCheck {
			continue
		}
		if !ne.Disable {
			controllerServices = append(controllerServices, ne)
		}
		exists := false
		for _, old := range *m.services {
			if ne.Name == old.Name {
				if ne.Disable {
					m.ctr.StopService(ne.Name)
					m.ctr.DisableService(ne.Name)
					restartCount++
				}
				if !ne.Equal(old) {
					logrus.Infof("Recreate service [%s]", ne.Name)
					if err := m.ctr.WriteConfig(ne); err == nil {
						m.ctr.EnableService(ne.Name)
						m.ctr.RestartService(ne.Name)
						restartCount++
					}
				} else {
					logrus.Infof("Service %s config no change", ne.Name)
				}
				exists = true
				break
			}
		}
		if !exists {
			logrus.Infof("Create service [%s]", ne.Name)
			if err := m.ctr.WriteConfig(ne); err == nil {
				m.ctr.EnableService(ne.Name)
				m.ctr.StartService(ne.Name)
				restartCount++
			}
		}
	}
	*m.allservice = services
	*m.services = controllerServices
	m.healthyManager.AddServicesAndUpdate(m.services)
	m.SyncServiceStatusController()
	logrus.Infof("load service config success, start or stop %d service and total %d service", restartCount, len(services))
	return nil
}

//StartService start a service
func (m *ManagerService) StartService(serviceName string) error {
	for _, service := range *m.services {
		if service.Name == serviceName {
			if !service.Disable {
				return fmt.Errorf("service %s is running", serviceName)
			}
			return m.ctr.StartService(serviceName)
		}
	}
	return nil
}

//StopService start a service
func (m *ManagerService) StopService(serviceName string) error {
	for i, service := range *m.services {
		if service.Name == serviceName {
			if service.Disable {
				return fmt.Errorf("service %s is stoped", serviceName)
			}
			(*m.services)[i].Disable = true
			m.lock.Lock()
			defer m.lock.Unlock()
			if controller, ok := m.autoStatusController[serviceName]; ok {
				controller.Stop()
			}
			return m.ctr.StopService(serviceName)
		}
	}
	return nil
}

//WriteServices write services
func (m *ManagerService) WriteServices() error {
	for _, s := range *m.services {
		if s.OnlyHealthCheck {
			continue
		}
		if s.Name == "docker" {
			continue
		}

		err := m.ctr.WriteConfig(s)
		if err != nil {
			return err
		}
	}

	return nil
}

//RemoveServices remove services
func (m *ManagerService) RemoveServices() error {
	for _, s := range *m.services {
		if s.Name == "docker" {
			continue
		}
		m.ctr.DisableService(s.Name)
		m.ctr.RemoveConfig(s.Name)
	}

	return nil
}

func isExistEndpoint(etcdEndPoints []string, end string) bool {
	for _, v := range etcdEndPoints {
		if v == end {
			return true
		}
	}
	return false
}

func rmEndpointFrom(etcdEndPoints []string, end string) []string {
	endPoints := make([]string, 0, 5)
	for _, v := range etcdEndPoints {
		if v != end {
			endPoints = append(endPoints, v)
		}
	}
	return endPoints
}

func toEndpoint(reg *service.Endpoint, ip string) string {
	if reg.Protocol == "" {
		return fmt.Sprintf("%s:%s", ip, reg.Port)
	}
	return fmt.Sprintf("%s://%s:%s", reg.Protocol, ip, reg.Port)
}

//InjectConfig inject config
func (m *ManagerService) InjectConfig(content string) string {
	for _, parantheses := range ArgsReg.FindAllString(content, -1) {
		logrus.Debugf("discover inject args template %s", parantheses)
		group := ArgsReg.FindStringSubmatch(parantheses)
		if group == nil || len(group) < 2 {
			logrus.Warnf("Not found group for %s", parantheses)
			continue
		}
		line := ""
		if group[1] == "NODE_UUID" {
			line = m.node.ID
		} else {
			endpoints := m.cluster.GetEndpoints(group[1])
			if len(endpoints) < 1 {
				logrus.Warnf("Failed to inject endpoints of key %s", group[1])
				continue
			}
			sep := ","
			if len(group) >= 3 && group[2] != "" {
				sep = group[2]
			}
			for _, end := range endpoints {
				if line == "" {
					line = end
				} else {
					line += sep
					line += end
				}
			}
		}
		content = strings.Replace(content, group[0], line, 1)
		logrus.Debugf("inject args into service %s => %v", group[1], line)
	}
	return content
}

//NewManagerService new controller manager
func NewManagerService(conf *option.Conf, healthyManager healthy.Manager, cluster client.ClusterClient) *ManagerService {
	ctx, cancel := context.WithCancel(context.Background())
	manager := &ManagerService{
		ctx:            ctx,
		cancel:         cancel,
		conf:           conf,
		cluster:        cluster,
		healthyManager: healthyManager,
		etcdcli:        conf.EtcdCli,
		services:       new([]*service.Service),
		allservice:     new([]*service.Service),
	}
	manager.ctr = NewController(conf, manager)
	return manager
}
