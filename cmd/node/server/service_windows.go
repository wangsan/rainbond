// RAINBOND, Application Management Platform
// Copyright (C) 2014-2017 Goodrain Co., Ltd.

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

package server

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/goodrain/rainbond/cmd/node/option"
	utilwindows "github.com/goodrain/rainbond/util/windows"
	"github.com/spf13/pflag"
	"golang.org/x/sys/windows"
)

var (
	flRegisterService   *bool
	flUnregisterService *bool
	flServiceName       *string

	setStdHandle = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetStdHandle")
	oldStderr    windows.Handle
	panicFile    *os.File
)

//InstallServiceFlags install service flag set
func InstallServiceFlags(flags *pflag.FlagSet) {
	flServiceName = flags.String("service-name", "rainbond-node", "Set the Windows service name")
	flRegisterService = flags.Bool("register-service", false, "Register the service and exit")
	flUnregisterService = flags.Bool("unregister-service", false, "Unregister the service and exit")
}
func getServicePath() (string, error) {
	p, err := exec.LookPath(os.Args[0])
	if err != nil {
		return "", err
	}
	return filepath.Abs(p)
}

// initService is the entry point for running the daemon as a Windows
// service. It returns an indication to stop (if registering/un-registering);
// an indication of whether it is running as a service; and an error.
func initService(conf *option.Conf) (bool, error) {
	if *flUnregisterService {
		if *flRegisterService {
			return true, errors.New("--register-service and --unregister-service cannot be used together")
		}
		return true, unregisterService()
	}

	if *flRegisterService {
		return true, registerService()
	}
	return false, nil
}

func unregisterService() error {
	return nil
}

func registerService() error {
	p, err := getServicePath()
	if err != nil {
		return err
	}
	// Configure the service to launch with the arguments that were just passed.
	args := []string{""}
	for _, a := range os.Args[1:] {
		if a != "--register-service" && a != "--unregister-service" {
			args = append(args, a)
		}
	}
	return utilwindows.RegisterService(*flServiceName, p, "Rainbond NodeManager", []string{}, args)
}
