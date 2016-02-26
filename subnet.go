// Example of minimal DHCP server:
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"net"
	"sync"
	"text/template"
	"time"

	dhcp "github.com/krolaw/dhcp4"
	"github.com/willf/bitset"
)

// Option id number from DHCP RFC 2132 and 2131
// Value is a string version of the value
type Option struct {
	Code  dhcp.OptionCode `json:"id"`
	Value string          `json:"value"`
}

func (o *Option) RenderToDHCP(srcOpts map[int]string) (code dhcp.OptionCode, val []byte, err error) {
	code = dhcp.OptionCode(o.Code)
	tmpl, err := template.New("dhcp_option").Parse(o.Value)
	if err != nil {
		return code, nil, err
	}
	buf := &bytes.Buffer{}
	if err := tmpl.Execute(buf, srcOpts); err != nil {
		return code, nil, err
	}
	val, err = convertOptionValueToByte(code, buf.String())
	return code, val, err
}

type Lease struct {
	Ip         net.IP    `json:"ip"`
	Mac        string    `json:"mac"`
	Valid      bool      `json:"valid"`
	ExpireTime time.Time `json:"expire_time"`
}

type Binding struct {
	Ip         net.IP    `json:"ip"`
	Mac        string    `json:"mac"`
	Options    []*Option `json:"options,omitempty"`
	NextServer *string   `json:"next_server,omitempty"`
}

type Subnet struct {
	sync.RWMutex
	Name              string
	Subnet            *MyIPNet
	NextServer        *net.IP `"json:,omitempty"`
	ActiveStart       net.IP
	ActiveEnd         net.IP
	ActiveLeaseTime   time.Duration
	ActiveBits        *bitset.BitSet
	ReservedLeaseTime time.Duration
	Leases            map[string]*Lease
	Bindings          map[string]*Binding
	Options           []*Option // Options to send to DHCP Clients
}

func NewSubnet() *Subnet {
	return &Subnet{
		Leases:     make(map[string]*Lease),
		Bindings:   make(map[string]*Binding),
		Options:    make([]*Option, 0),
		ActiveBits: bitset.New(0),
	}
}

type apiSubnet struct {
	Name              string     `json:"name"`
	Subnet            string     `json:"subnet"`
	NextServer        *string    `json:"next_server,omitempty"`
	ActiveStart       string     `json:"active_start"`
	ActiveEnd         string     `json:"active_end"`
	ActiveLeaseTime   int        `json:"active_lease_time"`
	ReservedLeaseTime int        `json:"reserved_lease_time"`
	Leases            []*Lease   `json:"leases,omitempty"`
	Bindings          []*Binding `json:"bindings,omitempty"`
	Options           []*Option  `json:"options,omitempty"`
}

func (s *Subnet) MarshalJSON() ([]byte, error) {
	s.RLock()
	defer s.RUnlock()
	as := &apiSubnet{
		Name:              s.Name,
		Subnet:            s.Subnet.String(),
		ActiveStart:       s.ActiveStart.String(),
		ActiveEnd:         s.ActiveEnd.String(),
		ActiveLeaseTime:   int(s.ActiveLeaseTime.Seconds()),
		ReservedLeaseTime: int(s.ReservedLeaseTime.Seconds()),
		Options:           s.Options,
		Leases:            make([]*Lease, len(s.Leases)),
		Bindings:          make([]*Binding, len(s.Bindings)),
	}
	if s.NextServer != nil {
		ns := s.NextServer.String()
		as.NextServer = &ns
	}
	i := int64(0)
	for _, lease := range s.Leases {
		as.Leases[i] = lease
		i++
	}
	i = int64(0)
	for _, binding := range s.Bindings {
		as.Bindings[i] = binding
		i++
	}
	return json.Marshal(as)
}

func (s *Subnet) UnmarshalJSON(data []byte) error {
	s.Lock()
	defer s.Unlock()
	as := &apiSubnet{}
	if err := json.Unmarshal(data, &as); err != nil {
		return err
	}
	s.Name = as.Name
	_, netdata, err := net.ParseCIDR(as.Subnet)
	if err != nil {
		return err
	} else {
		s.Subnet = &MyIPNet{netdata}
	}
	s.ActiveStart = net.ParseIP(as.ActiveStart).To4()
	s.ActiveEnd = net.ParseIP(as.ActiveEnd).To4()

	if !netdata.Contains(s.ActiveStart) {
		return errors.New("ActiveStart not in Subnet")
	}
	if !netdata.Contains(s.ActiveEnd) {
		return errors.New("ActiveEnd not in Subnet")
	}

	s.ActiveLeaseTime = time.Duration(as.ActiveLeaseTime) * time.Second
	s.ReservedLeaseTime = time.Duration(as.ReservedLeaseTime) * time.Second
	s.ActiveBits = bitset.New(uint(dhcp.IPRange(s.ActiveStart, s.ActiveEnd)))
	if as.NextServer != nil {
		ip := net.ParseIP(*as.NextServer).To4()
		s.NextServer = &ip
	}
	if s.ActiveLeaseTime == 0 {
		s.ActiveLeaseTime = 30 * time.Second
	}
	if s.ReservedLeaseTime == 0 {
		s.ReservedLeaseTime = 2 * time.Hour
	}
	if s.Leases == nil {
		s.Leases = map[string]*Lease{}
	}

	for _, v := range as.Leases {
		s.Leases[v.Mac] = v
		if dhcp.IPInRange(s.ActiveStart, s.ActiveEnd, v.Ip) {
			s.ActiveBits.Set(uint(dhcp.IPRange(s.ActiveStart, v.Ip) - 1))
		}
	}

	if s.Bindings == nil {
		s.Bindings = map[string]*Binding{}
	}

	for _, v := range as.Bindings {
		s.Bindings[v.Mac] = v
		if dhcp.IPInRange(s.ActiveStart, s.ActiveEnd, v.Ip) {
			s.ActiveBits.Set(uint(dhcp.IPRange(s.ActiveStart, v.Ip) - 1))
		}
	}

	s.Options = as.Options
	mask := net.IP([]byte(net.IP(netdata.Mask).To4()))
	bcastBits := binary.BigEndian.Uint32(netdata.IP) | ^binary.BigEndian.Uint32(mask)
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, bcastBits)
	s.Options = append(s.Options, &Option{dhcp.OptionSubnetMask, mask.String()})
	s.Options = append(s.Options, &Option{dhcp.OptionBroadcastAddress, net.IP(buf).String()})
	return nil
}

func (subnet *Subnet) free_lease(dt *DataTracker, nic string) {
	subnet.Lock()
	lease := subnet.Leases[nic]
	if lease != nil {
		if dhcp.IPInRange(subnet.ActiveStart, subnet.ActiveEnd, lease.Ip) {
			subnet.ActiveBits.Clear(uint(dhcp.IPRange(lease.Ip, subnet.ActiveStart) - 1))
		}
		delete(subnet.Leases, nic)
		subnet.Unlock()
		dt.save_data()
	} else {
		subnet.Unlock()
	}
}

func (subnet *Subnet) find_info(dt *DataTracker, nic string) (*Lease, *Binding) {
	subnet.RLock()
	l := subnet.Leases[nic]
	b := subnet.Bindings[nic]
	subnet.RUnlock()
	return l, b
}

func firstClearBit(bs *bitset.BitSet) (uint, bool) {
	for i := uint(0); i < bs.Len(); i++ {
		if !bs.Test(i) {
			return i, true
		}
	}
	return 0, false
}

// Assumes RWLock is held
func (subnet *Subnet) getFreeIP() (*net.IP, bool) {
	bit, success := firstClearBit(subnet.ActiveBits)
	if success {
		subnet.ActiveBits.Set(bit)
		ip := dhcp.IPAdd(subnet.ActiveStart, int(bit))
		return &ip, true
	}

	// Free invalid or expired leases
	save_me := false
	now := time.Now()
	for k, lease := range subnet.Leases {
		if now.After(lease.ExpireTime) {
			if dhcp.IPInRange(subnet.ActiveStart, subnet.ActiveEnd, lease.Ip) {
				subnet.ActiveBits.Clear(uint(dhcp.IPRange(lease.Ip, subnet.ActiveStart) - 1))
			}
			delete(subnet.Leases, k)
			save_me = true
		}
	}

	bit, success = firstClearBit(subnet.ActiveBits)
	if success {
		subnet.ActiveBits.Set(bit)
		ip := dhcp.IPAdd(subnet.ActiveStart, int(bit))
		return &ip, true
	}

	// We got nothin'
	return nil, save_me
}

func (subnet *Subnet) find_or_get_info(dt *DataTracker, nic string, suggest net.IP) (*Lease, *Binding) {
	// Fast path to see if we have a good lease
	subnet.RLock()
	binding := subnet.Bindings[nic]
	lease := subnet.Leases[nic]

	var theip *net.IP

	if binding != nil {
		theip = &binding.Ip
	}

	// Resolve potential conflicts.
	if lease != nil && binding != nil {
		if lease.Ip.Equal(binding.Ip) {
			subnet.RUnlock()
			return lease, binding
		}
		lease = nil
	}
	subnet.RUnlock()

	if lease == nil {
		// Slow path to see if we have can get a lease
		// Make sure nothing sneaked in
		subnet.Lock()
		lease = subnet.Leases[nic]
		binding = subnet.Bindings[nic]
		theip = nil
		if binding != nil {
			theip = &binding.Ip
		}
		// Resolve potential conflicts.
		if lease != nil && binding != nil {
			if lease.Ip.Equal(binding.Ip) {
				subnet.Unlock()
				return lease, binding
			}
		}

		if theip == nil {
			var save_me bool
			theip, save_me = subnet.getFreeIP()
			if theip == nil {
				subnet.Unlock()
				if save_me {
					dt.save_data()
				}
				return nil, nil
			}
		}
		lease = &Lease{
			Ip:    *theip,
			Mac:   nic,
			Valid: true,
		}
		subnet.Leases[nic] = lease
		subnet.Unlock()
		dt.save_data()
	}

	return lease, binding
}

func (s *Subnet) update_lease_time(dt *DataTracker, lease *Lease, d time.Duration) {
	lease.ExpireTime = time.Now().Add(d)
	dt.save_data()
}

func (s *Subnet) build_options(lease *Lease, binding *Binding, p dhcp.Packet) (dhcp.Options, time.Duration) {
	var lt time.Duration
	if binding == nil {
		lt = s.ActiveLeaseTime
	} else {
		lt = s.ReservedLeaseTime
	}

	opts := make(dhcp.Options)
	srcOpts := map[int]string{}
	for c, v := range p.ParseOptions() {
		srcOpts[int(c)] = convertByteToOptionValue(c, v)
		log.Printf("Recieved option: %v: %v", c, srcOpts[int(c)])
	}

	// Build renewal / rebinding time options
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(lt)/2)
	opts[dhcp.OptionRenewalTimeValue] = b
	b = make([]byte, 4)
	binary.BigEndian.PutUint32(b, uint32(lt)*3/4)
	opts[dhcp.OptionRebindingTimeValue] = b

	// fold in subnet options
	for _, opt := range s.Options {
		c, v, err := opt.RenderToDHCP(srcOpts)
		if err != nil {
			log.Printf("Failed to render option %v: %v, %v\n", opt.Code, opt.Value, err)
			continue
		}
		opts[c] = v
	}

	// fold in binding options
	if binding != nil {
		for _, opt := range binding.Options {
			c, v, err := opt.RenderToDHCP(srcOpts)
			if err != nil {
				log.Printf("Failed to render option %v: %v, %v\n", opt.Code, opt.Value, err)
				continue
			}
			opts[c] = v
		}
	}

	return opts, lt
}
