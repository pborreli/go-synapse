package synapse

import (
	log "github.com/Sirupsen/logrus"
	"encoding/json"
	"io/ioutil"
	"time"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"errors"
	"net"
)

type HAProxy struct {
	Configuration SynapseHAProxyConfiguration
	Backends HAProxyBackendSlice
	Services []SynapseService
	StateFile string
	WriteInterval int
}

func(h *HAProxy) Initialize(conf SynapseHAProxyConfiguration, Services []SynapseService, StateFile string, WriteInterval int) {
	h.Configuration = conf
	h.Services = Services
	h.StateFile = StateFile
	if WriteInterval > 0 {
		h.WriteInterval = WriteInterval
	}else {
		//1 second
		h.WriteInterval = 1000
	}
}

func(h *HAProxy) isBackendsModified(newBackends HAProxyBackendSlice) (bool,bool,[]string,error) {
	isModified := false
	hasToRestart := false
	var socketCommands []string
	//Compare current and new Backends (and all associated states)
	if len(newBackends) != len(h.Services) {
		err := errors.New("[" + strconv.Itoa(len(newBackends)) + "] Backends to watch != [" + strconv.Itoa(len(h.Services)) + "] Services")
		return isModified, hasToRestart, socketCommands, err
	}else {
		if len(newBackends) == len(h.Backends) {
			for index, backend := range newBackends {
				if len(backend.Servers) != len(h.Backends[index].Servers) {
					isModified = true
					hasToRestart = true
				}else {
					for i, server := range backend.Servers{
						if server.Name == h.Backends[index].Servers[i].Name && server.Host == h.Backends[index].Servers[i].Host && server.Port == h.Backends[index].Servers[i].Port {
							if server.Disabled != h.Backends[index].Servers[i].Disabled {
								isModified = true
								if server.Disabled {
									socketCommands = append(socketCommands,"disable server " + backend.Name + "/" + server.Name)
									log.Debug("disable server " + backend.Name + "/" + server.Name)
								}else {
									socketCommands = append(socketCommands,"enable server " + backend.Name + "/" + server.Name)
									log.Debug("enable server " + backend.Name + "/" + server.Name)
								}
							}
						}else {
							isModified = true
							hasToRestart = true
						}
					}
				}
			}
		}else {
			isModified = true
			hasToRestart = true
		}
	}
	return isModified, hasToRestart, socketCommands, nil
}

func(h *HAProxy) getAllBackends() HAProxyBackendSlice {
	var backends HAProxyBackendSlice
	for _, service := range h.Services {
		var backend HAProxyBackend
		backend.Name = service.Name
		backend.Port = service.HAPPort
		backend.ServerOptions = service.HAPServerOptions
		backend.Listen = service.HAPListen
		//Get All default servers to include
		for _, server := range service.DefaultServers {
			var hapServer HAProxyBackendServer
			hapServer.Host = server.Host
			hapServer.Port = server.Port
			hapServer.Name = server.Name
			hapServer.Disabled = false
			hapServer.Weight = 0
			backend.Servers = append(backend.Servers,hapServer)
		}
		//Get All dynamic servers to include
		discoveredHosts := service.Discovery.GetDiscoveredHosts()
		for _, server := range discoveredHosts {
			var hapServer HAProxyBackendServer
			hapServer.Host = server.Host
			hapServer.Port = server.Port
			hapServer.Name = server.Name
			hapServer.Disabled = server.Maintenance
			hapServer.Weight = server.Weight
			hapServer.HAProxyServerOptions = server.HAProxyServerOptions
			backend.Servers = append(backend.Servers,hapServer)
		}
		sort.Sort(backend.Servers)
		backends = append(backends, backend)
	}
	sort.Sort(backends)
	return backends
}

//Save the current state of all Backends to StateFile
func(h *HAProxy) SaveState() error {
	data, err := json.Marshal(h.Backends)
	if err != nil {
		log.WithError(err).Warn("Unable to Marchal in JSON Backends State")
		return err
	}
	err = ioutil.WriteFile(h.StateFile,data,0644)
	if err != nil {
		log.WithField("Filename",h.StateFile).WithError(err).Warn("Unable to Write Backends State into File")
		return err
	}
	return nil
}

//Load the current state of all Backends from StateFile
func(h *HAProxy) LoadState() error {
	if stat, err := os.Stat(h.StateFile); err == nil {
		fileModTime := stat.ModTime()
		now := time.Now()
		ttl := 2000
		expirationDate := fileModTime.Add(time.Duration(ttl) * time.Millisecond)
		if expirationDate.Before(now) {
			log.Debug("State File exists, but is expired")
			return nil
		}

		// Open and read the configuration file
		file, err := ioutil.ReadFile(h.StateFile)
		if err != nil {
			// If there is an error with opening or reading the configuration file, return the error, and an empty configuration object
			return err
		}

		// Trying to convert the content of the configuration file (theoriticaly in JSON) into a configuration object
		err = json.Unmarshal(file, &h.Backends)
		if err != nil {
			// If there is an error in decoding the JSON entry into configuration object, return a partialy unmarshalled object, and the error
			return err
		}
	}else {
		log.Debug("State File does not exists")
	}

	return nil
}

func(h *HAProxy) SaveConfiguration() error {
	if h.Configuration.DoWrites {
		var data string
	// Write Header
		data = "#\n"
		data += "# HAProxy Configuration File Generated by GO-Synapse\n"
		data += "# If you modify it, be aware that you modifications will be overriden soon\n"
		data += "#\n\n"
	// Global Section
		data += "global\n"
		for _, line := range h.Configuration.Global {
			data += "  " + line + "\n"
		}
	// Defaults Section
		data += "\ndefaults\n"
		for _, line := range h.Configuration.Defaults {
			data += "  " + line + "\n"
		}
		data += "\n"
	// Backend Section
		for _, backend := range h.Backends {
			data += "backend " + backend.Name + "\n"
			data += "  bind " + strconv.Itoa(backend.Port) + "\n"
			for _, line := range backend.Listen {
				data += "  " + line + "\n"
			}
			for _, server := range backend.Servers {
				data += "  server "
				data += server.Name + " "
				data += server.Host + ":"
				data += strconv.Itoa(server.Port) + " "
				data += backend.ServerOptions
				if server.HAProxyServerOptions != "" {
					data += " " + server.HAProxyServerOptions
				}
				if server.Disabled {
					data += " disabled"
				}
				data += "\n"
			}
			data += "\n"
		}
		err := ioutil.WriteFile(h.Configuration.ConfigFilePath,[]byte(data),0644)
		if err != nil {
			log.WithField("Filename",h.Configuration.ConfigFilePath).WithError(err).Warn("Unable to Write HAProxy Configuration File")
			return err
		}
	}else {
		log.Debug("Do not execute Write modified configuration cause of do_writes flag set to false")
	}
	return nil
}

func(h *HAProxy) reloadHAProxyDaemon() error {
	if h.Configuration.DoReloads {
		var command exec.Cmd
		command.Path = h.Configuration.ReloadCommand.Binary
		command.Args = h.Configuration.ReloadCommand.Arguments
		err := command.Run()
		if err != nil {
			log.WithError(err).Warn("HAProxy reloading failed")
			return err
		}
	}else {
		log.Debug("Do not execute restart cause of do_reloads flag set to false")
	}
	return nil
}

func(h *HAProxy) changeBackendsStateBySocket(commands []string) error {
	if h.Configuration.DoSocket {
		//Send all command to socket
		conn, err := net.Dial("unix",h.Configuration.SocketFilePath)
		if err != nil {
			log.WithError(err).Warn("Unable to open HAProxy socket to send new backend state")
			return err
		}
		for _, command := range commands {
			_, err = conn.Write([]byte(command))
			if err != nil {
				log.WithError(err).WithField("command",command).Warn("Unable to write command to HAProxy socket")
				conn.Close()
				return err
			}
			buf := make([]byte, 1024)
			n, err := conn.Read(buf[:])
			if err != nil {
				log.WithError(err).WithField("command",command).Warn("Unable to read after command from HAProxy socket")
				conn.Close()
				return err
			}
			return_string := string(buf[0:n])
			if return_string != "\n" {
				log.WithField("command",command).Warn("Unknown error after sending command from HAProxy socket[" + return_string + "]")
				conn.Close()
				return err
			}
		}
		conn.Close()
	}else {
		log.Debug("Do not send modified state to HAProxy cause of do_socket flag set to false")
	}
	return nil
}

func(h *HAProxy) Run(stop <-chan bool) {
	defer servicesWaitGroup.Done()
	//First Run, load first backends state from StateFile
	err := h.LoadState()
	if err != nil {
		log.WithError(err).Warn("Unable to load Backends State from file")
		log.Warn("Starting with an empty State")
	}
	//Now the main loop
	Loop:
	for {
		//First Get all backends info
		backends := h.getAllBackends()
		isModified, hasToRestart, socketCommands, err := h.isBackendsModified(backends)
		if err != nil {
			log.WithError(err).Warn("Error in modification since last check")
			log.Warn("Pretending there's no modification, to keep last valid state informations")
		}else {
			if isModified {
				log.Debug("Backends Configuration modified")
				//Save the new Backend State
				h.Backends = backends
				if h.StateFile != "" {
					h.SaveState()
				}
				//Write the new Configuration file
				err = h.SaveConfiguration()
				if err != nil {
					log.WithError(err).Warn("Unable to Save HAProxy Configuration File")
				}else {
					if hasToRestart {
						//Let's reload the main HAProxy process
						h.reloadHAProxyDaemon()
					}else {
						if h.Configuration.DoReloads && !h.Configuration.DoSocket {
							//HAProxy backend state modification by socket forbid by conf
							//So reload the modifications by restarting the daemon
							h.reloadHAProxyDaemon()
						}else {
							//Send command to haproxy using the control socket
							err = h.changeBackendsStateBySocket(socketCommands)
							if err != nil {
								h.reloadHAProxyDaemon()
							}
						}
					}
				}
			}else {
				log.Debug("No modification since last check, nothing to do")
			}
		}
		select {
		case <-stop:
			break Loop
		default:
			time.Sleep(time.Duration(h.WriteInterval) * time.Millisecond)
		}
	}
	log.Warn("HAProxy Management Routine stopped")
}
