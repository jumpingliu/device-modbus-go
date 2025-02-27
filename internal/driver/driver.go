// -*- Mode: Go; indent-tabs-mode: t -*-
//
// Copyright (C) 2018-2025 IOTech Ltd
//
// SPDX-License-Identifier: Apache-2.0

// Package driver is used to execute device-sdk's commands
package driver

import (
	"fmt"
	"sync"
	"time"

	"github.com/edgexfoundry/device-sdk-go/v4/pkg/interfaces"
	sdkModel "github.com/edgexfoundry/device-sdk-go/v4/pkg/models"
	"github.com/edgexfoundry/go-mod-core-contracts/v4/clients/logger"
	"github.com/edgexfoundry/go-mod-core-contracts/v4/models"
)

var once sync.Once
var driver *Driver

type Driver struct {
	Logger              logger.LoggingClient
	AsyncCh             chan<- *sdkModel.AsyncValues
	addressMutex        sync.Mutex
	addressMap          map[string]chan bool
	workingAddressCount map[string]int
	stopped             bool
	clientMutex         sync.Mutex
	clientMap           map[string]DeviceClient
}

var concurrentCommandLimit = 100

func (d *Driver) createDeviceClient(info *ConnectionInfo) (DeviceClient, error) {
	d.clientMutex.Lock()
	defer d.clientMutex.Unlock()
	key := info.String()
	c, ok := d.clientMap[key]
	if ok {
		return c, nil
	}
	c, err := NewDeviceClient(info)
	if err != nil {
		driver.Logger.Errorf("create device client failed. err:%v \n", err)
		return nil, err
	}
	d.clientMap[key] = c
	return c, nil
}

func (d *Driver) removeDeviceClient(info *ConnectionInfo) {
	d.clientMutex.Lock()
	defer d.clientMutex.Unlock()
	key := info.String()
	delete(d.clientMap, key)
}

func (d *Driver) DisconnectDevice(deviceName string, protocols map[string]models.ProtocolProperties) error {
	d.Logger.Warn("Driver's DisconnectDevice function didn't implement")
	return nil
}

// lockAddress mark address is unavailable because real device handle one request at a time
func (d *Driver) lockAddress(address string) error {
	if d.stopped {
		return fmt.Errorf("service attempts to stop and unable to handle new request")
	}
	d.addressMutex.Lock()
	lock, ok := d.addressMap[address]
	if !ok {
		lock = make(chan bool, 1)
		d.addressMap[address] = lock
	}

	// workingAddressCount used to check high-frequency command execution to avoid goroutine block
	count, ok := d.workingAddressCount[address]
	if !ok {
		d.workingAddressCount[address] = 1
	} else if count >= concurrentCommandLimit {
		d.addressMutex.Unlock()
		errorMessage := fmt.Sprintf("High-frequency command execution. There are %v commands with the same address in the queue", concurrentCommandLimit)
		d.Logger.Error(errorMessage)
		return fmt.Errorf("%s", errorMessage)
	} else {
		d.workingAddressCount[address] = count + 1
	}

	d.addressMutex.Unlock()
	lock <- true

	return nil
}

// unlockAddress remove token after command finish
func (d *Driver) unlockAddress(address string) {
	d.addressMutex.Lock()
	lock := d.addressMap[address]
	d.workingAddressCount[address] = d.workingAddressCount[address] - 1
	d.addressMutex.Unlock()
	<-lock
}

// lockableAddress return the lockable address according to the protocol
func (d *Driver) lockableAddress(info *ConnectionInfo) string {
	var address string
	if info.Protocol == ProtocolTCP {
		address = fmt.Sprintf("%s:%d", info.Address, info.Port)
	} else {
		address = info.Address
	}
	return address
}

func (d *Driver) HandleReadCommands(deviceName string, protocols map[string]models.ProtocolProperties, reqs []sdkModel.CommandRequest) (responses []*sdkModel.CommandValue, err error) {
	connectionInfo, err := createConnectionInfo(protocols)
	if err != nil {
		driver.Logger.Errorf("Fail to create read command connection info. err:%v \n", err)
		return responses, err
	}

	err = d.lockAddress(d.lockableAddress(connectionInfo))
	if err != nil {
		return responses, err
	}
	defer d.unlockAddress(d.lockableAddress(connectionInfo))

	responses = make([]*sdkModel.CommandValue, len(reqs))

	// create device client and open connection
	deviceClient, err := d.createDeviceClient(connectionInfo)
	if err != nil {
		driver.Logger.Errorf("Read command OpenConnection failed. err:%v \n", err)
		return responses, err
	}
	driver.Logger.Debugf("key = %s,client = %+v", connectionInfo.String(), deviceClient)

	// handle command requests
	for i, req := range reqs {
		res, err := handleReadCommandRequest(deviceClient, req)
		if err != nil {
			driver.Logger.Errorf("Read command failed, remove the Modbus client from the cache to allow re-establish connection next time. Cmd: %v err:%v", req.DeviceResourceName, err)
			d.removeDeviceClient(connectionInfo)
			return responses, err
		}

		responses[i] = res
	}
	driver.Logger.Debugf("get response %v", responses)
	return responses, nil
}

func handleReadCommandRequest(deviceClient DeviceClient, req sdkModel.CommandRequest) (*sdkModel.CommandValue, error) {
	var response []byte
	var result = &sdkModel.CommandValue{}
	var err error

	commandInfo, err := createCommandInfo(&req)
	if err != nil {
		return nil, err
	}

	response, err = deviceClient.GetValue(commandInfo)
	if err != nil {
		return result, err
	}

	result, err = TransformDataBytesToResult(&req, response, commandInfo)

	if err != nil {
		return result, err
	} else {
		driver.Logger.Tracef("Read command finished. Cmd:%v, %v \n", req.DeviceResourceName, result)
	}

	return result, nil
}

func (d *Driver) HandleWriteCommands(deviceName string, protocols map[string]models.ProtocolProperties, reqs []sdkModel.CommandRequest, params []*sdkModel.CommandValue) error {
	connectionInfo, err := createConnectionInfo(protocols)
	if err != nil {
		driver.Logger.Errorf("Fail to create write command connection info. err:%v \n", err)
		return err
	}

	err = d.lockAddress(d.lockableAddress(connectionInfo))
	if err != nil {
		return err
	}
	defer d.unlockAddress(d.lockableAddress(connectionInfo))

	// create device client and open connection
	deviceClient, err := d.createDeviceClient(connectionInfo)
	if err != nil {
		return err
	}

	// handle command requests
	for i, req := range reqs {
		err = handleWriteCommandRequest(deviceClient, req, params[i])
		if err != nil {
			driver.Logger.Errorf("Write command failed, remove the Modbus client from the cache to allow re-establish connection next time. Cmd: %v err:%v", req.DeviceResourceName, err)
			d.removeDeviceClient(connectionInfo)
			break
		}
	}

	return err
}

func handleWriteCommandRequest(deviceClient DeviceClient, req sdkModel.CommandRequest, param *sdkModel.CommandValue) error {
	var err error

	commandInfo, err := createCommandInfo(&req)
	if err != nil {
		return err
	}

	dataBytes, err := TransformCommandValueToDataBytes(commandInfo, param)
	if err != nil {
		return fmt.Errorf("transform command value failed, err: %v", err)
	}

	err = deviceClient.SetValue(commandInfo, dataBytes)
	if err != nil {
		return fmt.Errorf("handle write command request failed, err: %v", err)
	}

	driver.Logger.Tracef("Write command finished. Cmd:%v \n", req.DeviceResourceName)
	return nil
}

func (d *Driver) Initialize(sdk interfaces.DeviceServiceSDK) error {
	d.Logger = sdk.LoggingClient()
	d.AsyncCh = sdk.AsyncValuesChannel()
	d.addressMap = make(map[string]chan bool)
	d.workingAddressCount = make(map[string]int)
	d.clientMap = make(map[string]DeviceClient)
	return nil
}

func (d *Driver) Start() error {
	return nil
}

func (d *Driver) Stop(force bool) error {
	d.clientMutex.Lock()
	for key, client := range d.clientMap {
		err := client.CloseConnection()
		if err != nil {
			d.Logger.Errorf("device client closed,client key = %s,err = %v", key, err)
		}
	}
	d.clientMutex.Unlock()
	d.stopped = true
	if !force {
		d.waitAllCommandsToFinish()
	}
	for _, locked := range d.addressMap {
		close(locked)
	}
	return nil
}

// waitAllCommandsToFinish used to check and wait for the unfinished job
func (d *Driver) waitAllCommandsToFinish() {
loop:
	for {
		for _, count := range d.workingAddressCount {
			if count != 0 {
				// wait a moment and check again
				time.Sleep(time.Second * SERVICE_STOP_WAIT_TIME)
				continue loop
			}
		}
		break loop
	}
}

func (d *Driver) AddDevice(deviceName string, protocols map[string]models.ProtocolProperties, adminState models.AdminState) error {
	d.Logger.Debugf("Device %s is added", deviceName)
	return nil
}

func (d *Driver) UpdateDevice(deviceName string, protocols map[string]models.ProtocolProperties, adminState models.AdminState) error {
	d.Logger.Debugf("Device %s is updated", deviceName)
	return nil
}

func (d *Driver) RemoveDevice(deviceName string, protocols map[string]models.ProtocolProperties) error {
	d.Logger.Debugf("Device %s is removed", deviceName)
	return nil
}

func (d *Driver) Discover() error {
	return fmt.Errorf("driver's Discover function isn't implemented")
}

func (d *Driver) ValidateDevice(device models.Device) error {
	_, err := createConnectionInfo(device.Protocols)
	if err != nil {
		return fmt.Errorf("invalid protocol properties, %v", err)
	}
	return nil
}

func NewProtocolDriver() interfaces.ProtocolDriver {
	once.Do(func() {
		driver = new(Driver)
	})
	return driver
}
