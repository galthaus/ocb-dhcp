package main

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"sync"

	dhcp "github.com/krolaw/dhcp4"
)

type DataTracker struct {
	Subnets  map[string]*Subnet // subnet -> SubnetData
	data_dir string             `json:"-"`
	lock     sync.Mutex         `json:"-"`
}

func NewDataTracker(data_dir string) *DataTracker {
	return &DataTracker{
		Subnets:  make(map[string]*Subnet),
		data_dir: data_dir,
	}
}

func (dt *DataTracker) FindBoundIP(mac net.HardwareAddr) *Subnet {
	for _, s := range dt.Subnets {
		for _, b := range s.Bindings {
			if b.Mac == mac.String() {
				return s
			}
		}
	}
	return nil
}

func (dt *DataTracker) FindSubnet(ip net.IP) *Subnet {
	for _, s := range dt.Subnets {
		if s.Subnet.Contains(ip) {
			return s
		}
	}
	return nil
}

func (dt *DataTracker) AddSubnet(s *Subnet) (error, int) {
	lsubnet := dt.Subnets[s.Name]
	if lsubnet != nil {
		return errors.New("Already exists"), http.StatusConflict
	}

	// Make sure subnet doesn't overlap into other spaces.
	if dt.subnetsOverlap(s) {
		return errors.New("Subnet overlaps with existing subnet"), http.StatusBadRequest
	}

	dt.Subnets[s.Name] = s
	dt.save_data()
	return nil, http.StatusOK
}

func (dt *DataTracker) RemoveSubnet(subnetName string) (error, int) {
	lsubnet := dt.Subnets[subnetName]
	if lsubnet == nil {
		return errors.New("Not Found"), http.StatusNotFound
	}
	delete(dt.Subnets, subnetName)
	dt.save_data()
	return nil, http.StatusOK
}

func (dt *DataTracker) ReplaceSubnet(subnetName string, subnet *Subnet) (error, int) {
	lsubnet := dt.Subnets[subnetName]
	if lsubnet == nil {
		return errors.New("Not Found"), http.StatusNotFound
	}

	// Take Leases and Bindings from old to new if nets match
	subnet.Leases = lsubnet.Leases
	subnet.Bindings = lsubnet.Bindings

	// XXX: One day we should handle if active/reserved change.
	subnet.ActiveBits = lsubnet.ActiveBits

	delete(dt.Subnets, lsubnet.Name)

	// Make sure subnet doesn't overlap into other spaces.
	if dt.subnetsOverlap(subnet) {
		// Put the original back
		dt.Subnets[lsubnet.Name] = lsubnet
		return errors.New("Subnet overlaps with existing subnet"), http.StatusBadRequest
	}

	dt.Subnets[subnet.Name] = subnet
	dt.save_data()
	return nil, http.StatusOK
}

// HACK BECAUSE IPNet doesn't marshall/unmarshall
type MyIPNet struct {
	*net.IPNet
}

func (ipnet MyIPNet) MarshalText() ([]byte, error) {
	return []byte(ipnet.String()), nil
}

// UnmarshalText implements the encoding.TextUnmarshaler interfacee.
// The IP address is expected in a form accepted by ParseIP.
func (ipnet *MyIPNet) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		return errors.New("Empty MyIPNet")
	}
	s := string(text)
	_, newnet, err := net.ParseCIDR(s)
	if err != nil {
		return &net.ParseError{"NetIP address", s}
	}
	*ipnet = MyIPNet{
		&net.IPNet{IP: newnet.IP, Mask: newnet.Mask},
	}
	return nil
}

/*
 * Data storage/retrieval functions
 */
func (dt *DataTracker) load_data() {
	dt.lock.Lock()
	bytes, err := ioutil.ReadFile(dt.data_dir + "/database.json")
	if err != nil {
		log.Panic("failed to read file", err.Error())
	}

	err = json.Unmarshal(bytes, dt)
	if err != nil {
		log.Panic("failed to parse file", err.Error())
	}
	dt.lock.Unlock()
}

func (dt *DataTracker) save_data() {
	dt.lock.Lock()
	jdata, err := json.Marshal(dt)
	if err != nil {
		log.Panic("Failed to marshal data", err.Error())
	}
	err = ioutil.WriteFile(dt.data_dir+"/database.json", jdata, 0700)
	if err != nil {
		log.Panic("Failed to save data", err.Error())
	}
	dt.lock.Unlock()
}

func (dt *DataTracker) subnetsOverlap(subnet *Subnet) bool {
	for _, es := range dt.Subnets {
		if es.Subnet.Contains(subnet.Subnet.IP) {
			return true
		}
		if subnet.Subnet.Contains(es.Subnet.IP) {
			return true
		}
	}
	return false
}

func (dt *DataTracker) AddBinding(subnetName string, binding Binding) (error, int) {
	lsubnet := dt.Subnets[subnetName]
	if lsubnet == nil {
		return errors.New("Not Found"), http.StatusNotFound
	}

	// If existing, clear the reservation for IP
	b := lsubnet.Bindings[binding.Mac]
	if b != nil {
		if dhcp.IPInRange(lsubnet.ActiveStart, lsubnet.ActiveEnd, b.Ip) {
			lsubnet.ActiveBits.Clear(uint(dhcp.IPRange(lsubnet.ActiveStart, b.Ip) - 1))
		}
	}

	// Reserve the IP if in Active range
	if dhcp.IPInRange(lsubnet.ActiveStart, lsubnet.ActiveEnd, binding.Ip) {
		lsubnet.ActiveBits.Set(uint(dhcp.IPRange(lsubnet.ActiveStart, binding.Ip) - 1))
	}

	lsubnet.Bindings[binding.Mac] = &binding
	dt.save_data()
	return nil, http.StatusOK
}

func (dt *DataTracker) DeleteBinding(subnetName, mac string) (error, int) {
	lsubnet := dt.Subnets[subnetName]
	if lsubnet == nil {
		return errors.New("Subnet Not Found"), http.StatusNotFound
	}

	b := lsubnet.Bindings[mac]
	if b == nil {
		return errors.New("Binding Not Found"), http.StatusNotFound
	}

	if dhcp.IPInRange(lsubnet.ActiveStart, lsubnet.ActiveEnd, b.Ip) {
		lsubnet.ActiveBits.Clear(uint(dhcp.IPRange(lsubnet.ActiveStart, b.Ip) - 1))
	}

	delete(lsubnet.Bindings, mac)
	dt.save_data()
	return nil, http.StatusOK
}

func (dt *DataTracker) SetNextServer(subnetName string, ip net.IP, nextServer NextServer) (error, int) {
	lsubnet := dt.Subnets[subnetName]
	if lsubnet == nil {
		return errors.New("Not Found"), http.StatusNotFound
	}

	save_me := false
	for _, v := range lsubnet.Bindings {
		if v.Ip.Equal(ip) && (v.NextServer == nil || *v.NextServer != nextServer.Server) {
			save_me = true
			v.NextServer = &nextServer.Server
		}
	}

	if save_me {
		dt.save_data()
	}

	return nil, http.StatusOK
}
