// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ambient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strings"

	"github.com/vishvananda/netlink"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"istio.io/istio/cni/pkg/ambient/constants"
	"istio.io/istio/pkg/offmesh"
	istiolog "istio.io/pkg/log"
)

var log = istiolog.RegisterScope("ambient", "ambient controller", 0)

func IsPodInIpset(pod *corev1.Pod) bool {
	ipset, err := Ipset.List()
	if err != nil {
		log.Errorf("Failed to list ipset entries: %v", err)
		return false
	}

	// Since not all kernels support comments in ipset, we should also try and
	// match against the IP
	for _, ip := range ipset {
		if ip.Comment == string(pod.UID) {
			return true
		}
		if ip.IP.String() == pod.Status.PodIP {
			return true
		}
	}

	return false
}

func RouteExists(rte []string) bool {
	output, err := executeOutput(
		"bash", "-c",
		fmt.Sprintf("ip route show %s | wc -l", strings.Join(rte, " ")),
	)
	if err != nil {
		return false
	}

	log.Debugf("RouteExists(%s): %s", strings.Join(rte, " "), output)

	return output == "1"
}

func AddPodToMesh(pod *corev1.Pod, ip string) {
	if ip == "" {
		ip = pod.Status.PodIP
	}

	if !IsPodInIpset(pod) {
		log.Infof("Adding pod '%s/%s' (%s) to ipset", pod.Name, pod.Namespace, string(pod.UID))
		err := Ipset.AddIP(net.ParseIP(ip).To4(), string(pod.UID))
		if err != nil {
			log.Errorf("Failed to add pod %s to ipset list: %v", pod.Name, err)
		}
	} else {
		log.Infof("Pod '%s/%s' (%s) is in ipset", pod.Name, pod.Namespace, string(pod.UID))
	}

	rte, err := buildRouteFromPod(pod, ip)
	if err != nil {
		log.Errorf("Failed to build route for pod %s: %v", pod.Name, err)
	}

	if !RouteExists(rte) {
		log.Infof("Adding route for %s/%s: %+v", pod.Name, pod.Namespace, rte)
		// @TODO Try and figure out why buildRouteFromPod doesn't return a good route that we can
		// use err = netlink.RouteAdd(rte):
		// Error: {"level":"error","time":"2022-06-24T16:30:59.083809Z","msg":"Failed to add route ({Ifindex: 4 Dst: 10.244.2.7/32
		// Via: Family: 2, Address: 192.168.126.2 Src: 10.244.2.1 Gw: <nil> Flags: [] Table: 100 Realm: 0}) for pod
		// helloworld-v2-same-node-67b6b764bf-zhmp4: invalid argument"}
		err = execute("ip", append([]string{"route", "add"}, rte...)...)
		if err != nil {
			log.Warnf("Failed to add route (%s) for pod %s: %v", rte, pod.Name, err)
		}
	} else {
		log.Infof("Route already exists for %s/%s: %+v", pod.Name, pod.Namespace, rte)
	}

	dev, err := getDeviceWithDestinationOf(ip)
	if err != nil {
		log.Warnf("Failed to get device for destination %s", ip)
		return
	}
	err = SetProc("/proc/sys/net/ipv4/conf/"+dev+"/rp_filter", "0")
	if err != nil {
		log.Warnf("Failed to set rp_filter to 0 for device %s", dev)
	}
}

func DelPodFromMesh(pod *corev1.Pod) {
	log.Debugf("Removing pod '%s/%s' (%s) from mesh", pod.Name, pod.Namespace, string(pod.UID))
	if IsPodInIpset(pod) {
		log.Infof("Removing pod '%s' (%s) from ipset", pod.Name, string(pod.UID))
		err := Ipset.DeleteIP(net.ParseIP(pod.Status.PodIP).To4())
		if err != nil {
			log.Errorf("Failed to delete pod %s from ipset list: %v", pod.Name, err)
		}
	} else {
		log.Infof("Pod '%s/%s' (%s) is not in ipset", pod.Name, pod.Namespace, string(pod.UID))
	}
	rte, err := buildRouteFromPod(pod, "")
	if err != nil {
		log.Errorf("Failed to build route for pod %s: %v", pod.Name, err)
	}
	if RouteExists(rte) {
		log.Infof("Removing route: %+v", rte)
		// @TODO Try and figure out why buildRouteFromPod doesn't return a good route that we can
		// use this:
		// err = netlink.RouteDel(rte)
		err = execute("ip", append([]string{"route", "del"}, rte...)...)
		if err != nil {
			log.Warnf("Failed to delete route (%s) for pod %s: %v", rte, pod.Name, err)
		}
	}
}

func buildRouteFromPod(pod *corev1.Pod, ip string) ([]string, error) {
	if ip == "" {
		ip = pod.Status.PodIP
	}

	if ip == "" {
		return nil, errors.New("no ip found")
	}

	return []string{
		"table",
		fmt.Sprintf("%d", constants.RouteTableInbound),
		fmt.Sprintf("%s/32", ip),
		"via",
		constants.ZTunnelInboundTunIP,
		"dev",
		constants.InboundTun,
		"src",
		HostIP,
	}, nil
}

func (s *Server) routesAdd(routes []*netlink.Route) error {
	for _, route := range routes {
		log.Debugf("Adding route: %+v", route)
		err := netlink.RouteAdd(route)
		if err != nil {
			return err
		}
	}

	return nil
}

func getDeviceWithDestinationOf(ip string) (string, error) {
	routes, err := netlink.RouteListFiltered(
		netlink.FAMILY_V4,
		&netlink.Route{Dst: &net.IPNet{IP: net.ParseIP(ip), Mask: net.CIDRMask(32, 32)}},
		netlink.RT_FILTER_DST)
	if err != nil {
		return "", err
	}

	if len(routes) == 0 {
		return "", errors.New("no routes found")
	}

	linkIndex := routes[0].LinkIndex
	link, err := netlink.LinkByIndex(linkIndex)
	if err != nil {
		return "", err
	}
	return link.Attrs().Name, nil
}

func GetHostNetDevice(hostIP string) (string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return "", err
	}
	for _, link := range links {
		addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			if addr.IP.String() == hostIP {
				return link.Attrs().Name, nil
			}
		}
	}
	return "", errors.New("not found")
}

func GetHostIP(kubeClient kubernetes.Interface) (string, error) {
	var ip string
	// Get the node from the Kubernetes API
	node, err := kubeClient.CoreV1().Nodes().Get(context.TODO(), NodeName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("error getting node: %v", err)
	}

	ip = node.Spec.PodCIDR
	fmt.Printf("node.Spec.PodCIDR: %v\n", ip)
	// This needs to be done as in Kind, the node internal IP is not the one we want.
	if ip == "" {
		// PodCIDR is not set, try to get the IP from the node internal IP
		fmt.Printf("node.Status.Addresses: %v\n", node.Status.Addresses)
		for _, address := range node.Status.Addresses {
			if address.Type == corev1.NodeInternalIP {
				return address.Address, nil
			}
		}
	} else {
		network, err := netip.ParsePrefix(ip)
		if err != nil {
			return "", fmt.Errorf("error parsing node IP: %v\n", err)
		}

		ifaces, err := net.Interfaces()
		if err != nil {
			return "", fmt.Errorf("error getting interfaces: %v\n", err)
		}
		for _, iface := range ifaces {
			addrs, err := iface.Addrs()
			fmt.Printf("iface: %v\n", iface)
			if err != nil {
				return "", fmt.Errorf("error getting addresses: %v", err)
			}

			for _, addr := range addrs {
				fmt.Printf("addr: %v\n", addr.String())
				a, err := netip.ParseAddr(strings.Split(addr.String(), "/")[0])
				if err != nil {
					return "", fmt.Errorf("error parsing address: %v", err)
				}
				if network.Contains(a) {
					return a.String(), nil
				}
			}
		}
	}

	return "", nil
}

// CreateRulesOnCPUNode initializes the routing, firewall and ipset rules on the node.
// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh
func (s *Server) CreateRulesOnCPUNode(cpuEth, ztunnelIP string, captureDNS bool) error {
	var err error

	log.Debugf("CreateRulesOnNode: cpuEth=%s, ztunnelIP=%s", cpuEth, ztunnelIP)

	// Check if chain exists, if it exists flush.. otherwise initialize
	// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L28
	err = execute(IptablesCmd, "-t", "mangle", "-C", "output", "-j", constants.ChainZTunnelOutput)
	if err == nil {
		log.Debugf("Chain %s already exists, flushing", constants.ChainOutput)
		s.flushLists()
	} else {
		log.Debugf("Initializing lists")
		err = s.initializeLists()
		if err != nil {
			return err
		}
	}

	// Create ipset of pod members.
	// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L85
	log.Debug("Creating ipset")
	err = Ipset.CreateSet()
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("error creating ipset: %v", err)
	}

	appendRules := []*iptablesRule{
		// Skip things that come from the tunnels, but don't apply the conn skip mark
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L88
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", constants.DPUTun,
			"-j", "MARK",
			"--set-mark", constants.SkipMark,
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L89
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", constants.DPUTun,
			"-j", "RETURN",
		),

		// Make sure that whatever is skipped is also skipped for returning packets.
		// If we have a skip mark, save it to conn mark.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L95
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelForward,
			"-m", "mark",
			"--mark", constants.ConnSkipMark,
			"-j", "CONNMARK",
			"--save-mark",
			"--nfmask", constants.ConnSkipMask,
			"--ctmask", constants.ConnSkipMask,
		),
		// Input chain might be needed for things in host namespace that are skipped.
		// Place the mark here after routing was done, not sure if conn-tracking will figure
		// it out if I do it before, as NAT might change the connection tuple.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L99
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelInput,
			"-m", "mark",
			"--mark", constants.ConnSkipMark,
			"-j", "CONNMARK",
			"--save-mark",
			"--nfmask", constants.ConnSkipMask,
			"--ctmask", constants.ConnSkipMask,
		),

		// For things with the proxy mark, we need different routing just on returning packets
		// so we give a different mark to them.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L103
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelForward,
			"-m", "mark",
			"--mark", constants.ProxyMark,
			"-j", "CONNMARK",
			"--save-mark",
			"--nfmask", constants.ProxyMask,
			"--ctmask", constants.ProxyMask,
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L104
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelInput,
			"-m", "mark",
			"--mark", constants.ProxyMark,
			"-j", "CONNMARK",
			"--save-mark",
			"--nfmask", constants.ProxyMask,
			"--ctmask", constants.ProxyMask,
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L106
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelOutput,
			"--source", HostIP,
			"-j", "MARK",
			"--set-mark", constants.ConnSkipMask,
		),

		// If we have an outbound mark, we don't need kube-proxy to do anything,
		// so accept it before kube-proxy translates service vips to pod ips
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L122
		newIptableRule(
			constants.TableNat,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.OutboundMark,
			"-j", "ACCEPT",
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L123
		newIptableRule(
			constants.TableNat,
			constants.ChainZTunnelPostrouting,
			"-m", "mark",
			"--mark", constants.OutboundMark,
			"-j", "ACCEPT",
		),
	}

	if captureDNS {
		appendRules = append(appendRules,
			newIptableRule(
				constants.TableNat,
				constants.ChainZTunnelPrerouting,
				"-p", "udp",
				"-m", "set",
				"--match-set", Ipset.Name, "src",
				"--dport", "53",
				"-j", "DNAT",
				"--to", fmt.Sprintf("%s:%d", ztunnelIP, constants.DNSCapturePort),
			),
		)
	}

	appendRules2 := []*iptablesRule{
		// Don't set anything on the tunnel (geneve port is 6081), as the tunnel copies
		// the mark to the un-tunneled packet.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L126
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-p", "udp",
			"-m", "udp",
			"--dport", "6081",
			"-j", "RETURN",
		),

		// If we have the conn mark, restore it to mark, to make sure that the other side of the connection
		// is skipped as well.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L129-L130
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "connmark",
			"--mark", constants.ConnSkipMark,
			"-j", "MARK",
			"--set-mark", constants.SkipMark,
		),
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.SkipMark,
			"-j", "RETURN",
		),

		// If we have the proxy mark in, set the return mark to make sure that original src packets go to ztunnel
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L133-L134
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"!", "-i", cpuEth,
			"-m", "connmark",
			"--mark", constants.ProxyMark,
			"-j", "MARK",
			"--set-mark", constants.ProxyRetMark,
		),
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.ProxyRetMark,
			"-j", "RETURN",
		),

		// Send fake source outbound connections to the outbound route table (for original src)
		// if it's original src, the source ip of packets coming from the proxy might be that of a pod, so
		// make sure we don't tproxy it
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L139-L140
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", cpuEth,
			"!", "--source", ztunnelIP,
			"--match-set", Ipset.Name, "dst",
			"-j", "MARK",
			"--set-mark", constants.ProxyMark,
		),
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.SkipMark,
			"-j", "RETURN",
		),

		// Make sure anything that leaves ztunnel is routed normally (xds, connections to other ztunnels,
		// connections to upstream pods...)
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L143
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", cpuEth,
			"--match-set", Ipset.Name, "dst",
			"-j", "MARK",
			"--set-mark", constants.ConnSkipMark,
		),

		// skip udp so DNS works. We can make this more granular.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L146
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-p", "udp",
			"-j", "MARK",
			"--set-mark", constants.ConnSkipMark,
		),

		// Skip things from host ip - these are usually kubectl probes
		// skip anything with skip mark. This can be used to add features like port exclusions
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L149
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.SkipMark,
			"-j", "RETURN",
		),

		// Mark outbound connections to route them to the proxy using ip rules/route tables
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L151
		// Per Yuval, interface_prefix can be left off this rule... but we should check this (hard to automate
		// detection).
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-p", "tcp",
			"-m", "set",
			"--match-set", Ipset.Name, "src",
			"-j", "MARK",
			"--set-mark", constants.OutboundMark,
		),
	}

	err = iptablesAppend(appendRules)
	if err != nil {
		log.Errorf("failed to append iptables rule: %v", err)
	}

	err = iptablesAppend(appendRules2)
	if err != nil {
		log.Errorf("failed to append iptables rule: %v", err)
	}

	// Need to do some work in procfs
	// @TODO: This likely needs to be cleaned up, there are a lot of martians in AWS
	// that seem to necessitate this work.
	procs := map[string]int{
		"/proc/sys/net/ipv4/conf/default/rp_filter":           0,
		"/proc/sys/net/ipv4/conf/all/rp_filter":               0,
		"/proc/sys/net/ipv4/conf/" + cpuEth + "/rp_filter":    0,
		"/proc/sys/net/ipv4/conf/" + cpuEth + "/accept_local": 1,
	}
	for proc, val := range procs {
		err = SetProc(proc, fmt.Sprint(val))
		if err != nil {
			log.Errorf("failed to write to proc file %s: %v", proc, err)
		}
	}

	// Create tunnels
	// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L153-L161
	dputun := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: constants.DPUTun,
		},
		ID:     1000,
		Remote: net.ParseIP(offmesh.GetPair(NodeName, offmesh.CPUNode, s.offmeshCluster).IP),
	}
	log.Debugf("Building dpu tunnel: %+v", dputun)
	err = netlink.LinkAdd(dputun)
	if err != nil {
		log.Errorf("failed to add dpu tunnel: %v", err)
	}
	err = netlink.AddrAdd(dputun, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   net.ParseIP(constants.CPUDPUTunIP),
			Mask: net.CIDRMask(constants.TunPrefix, 32),
		},
	})
	if err != nil {
		log.Errorf("failed to add dpu tunnel address: %v", err)
	}

	err = netlink.LinkSetUp(dputun)
	if err != nil {
		log.Errorf("failed to set dpu tunnel up: %v", err)
	}

	procs = map[string]int{
		"/proc/sys/net/ipv4/conf/" + constants.InboundTun + "/rp_filter":     0,
		"/proc/sys/net/ipv4/conf/" + constants.InboundTun + "/accept_local":  1,
		"/proc/sys/net/ipv4/conf/" + constants.OutboundTun + "/rp_filter":    0,
		"/proc/sys/net/ipv4/conf/" + constants.OutboundTun + "/accept_local": 1,
	}
	for proc, val := range procs {
		err = SetProc(proc, fmt.Sprint(val))
		if err != nil {
			log.Errorf("failed to write to proc file %s: %v", proc, err)
		}
	}

	dirEntries, err := os.ReadDir("/proc/sys/net/ipv4/conf")
	if err != nil {
		log.Errorf("failed to read /proc/sys/net/ipv4/conf: %v", err)
	}
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() {
			if _, err := os.Stat("/proc/sys/net/ipv4/conf/" + dirEntry.Name() + "/rp_filter"); err != nil {
				err := SetProc("/proc/sys/net/ipv4/conf/"+dirEntry.Name()+"/rp_filter", "0")
				if err != nil {
					log.Errorf("failed to set /proc/sys/net/ipv4/conf/%s/rp_filter: %v", dirEntry.Name(), err)
				}
			}
		}
	}

	routes := []*ExecList{
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L164
		//newExec("ip",
		//	[]string{
		//		"route", "add", "table", fmt.Sprint(constants.RouteTableOutbound), ztunnelIP,
		//		"dev", ztunnelVeth, "scope", "link",
		//	},
		//),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L166
		newExec("ip",
			[]string{
				"route", "add", "table", fmt.Sprint(constants.RouteTableOutbound), "0.0.0.0/0",
				"via", constants.DPUCPUTunIP, "dev", constants.DPUTun,
			},
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L168
		//newExec("ip",
		//	[]string{
		//		"route", "add", "table", fmt.Sprint(constants.RouteTableProxy), ztunnelIP,
		//		"dev", ztunnelVeth, "scope", "link",
		//	},
		//),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L169
		newExec("ip",
			[]string{
				"route", "add", "table", fmt.Sprint(constants.RouteTableProxy), "0.0.0.0/0",
				"via", offmesh.GetPair(NodeName, offmesh.CPUNode, s.offmeshCluster).IP, "dev", cpuEth,
			},
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L171
		//newExec("ip",
		//	[]string{
		//		"route", "add", "table", fmt.Sprint(constants.RouteTableInbound), ztunnelIP,
		//		"dev", ztunnelVeth, "scope", "link",
		//	},
		//),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L62-L77
		// Everything with the skip mark goes directly to the main table
		newExec("ip",
			[]string{
				"rule", "add", "priority", "100",
				"fwmark", fmt.Sprint(constants.SkipMark),
				"goto", "32766",
			},
		),
		// Everything with the outbound mark goes to the tunnel out device
		// using the outbound route table
		newExec("ip",
			[]string{
				"rule", "add", "priority", "101",
				"fwmark", fmt.Sprint(constants.OutboundMark),
				"lookup", fmt.Sprint(constants.RouteTableOutbound),
			},
		),
		// Things with the proxy return mark go directly to the proxy veth using the proxy
		// route table (useful for original src)
		newExec("ip",
			[]string{
				"rule", "add", "priority", "102",
				"fwmark", fmt.Sprint(constants.ProxyRetMark),
				"lookup", fmt.Sprint(constants.RouteTableProxy),
			},
		),
		// Send all traffic to the inbound table. This table has routes only to pods in the mesh.
		// It does not have a catch-all route, so if a route is missing, the search will continue
		// allowing us to override routing just for member pods.
		//newExec("ip",
		//	[]string{
		//		"rule", "add", "priority", "103",
		//		"table", fmt.Sprint(constants.RouteTableInbound),
		//	},
		//),
	}

	for _, route := range routes {
		err = execute(route.Cmd, route.Args...)
		if err != nil {
			log.Errorf(fmt.Errorf("failed to add route (%+v): %v", route, err))
		}
	}

	return nil
}

func (s *Server) cleanup() {
	log.Infof("server terminated, cleaning up")
	s.cleanRules()

	// Clean up ip route tables
	_ = routeFlushTable(constants.RouteTableInbound)
	_ = routeFlushTable(constants.RouteTableOutbound)
	_ = routeFlushTable(constants.RouteTableProxy)

	exec := []*ExecList{
		newExec("ip", []string{"rule", "del", "priority", "100"}),
		newExec("ip", []string{"rule", "del", "priority", "101"}),
		newExec("ip", []string{"rule", "del", "priority", "102"}),
		newExec("ip", []string{"rule", "del", "priority", "103"}),
	}
	for _, e := range exec {
		err := execute(e.Cmd, e.Args...)
		if err != nil {
			log.Warnf("Error running command %v %v: %v", e.Cmd, strings.Join(e.Args, " "), err)
		}
	}

	// Delete tunnel links
	err := netlink.LinkDel(&netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: constants.InboundTun,
		},
	})
	if err != nil {
		log.Warnf("error deleting inbound tunnel: %v", err)
	}
	err = netlink.LinkDel(&netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: constants.OutboundTun,
		},
	})
	if err != nil {
		log.Warnf("error deleting outbound tunnel: %v", err)
	}

	_ = Ipset.DestroySet()
}

func routeFlushTable(table int) error {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &netlink.Route{Table: table}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return err
	}
	err = routesDelete(routes)
	if err != nil {
		return err
	}
	return nil
}

func routesDelete(routes []netlink.Route) error {
	for _, r := range routes {
		err := netlink.RouteDel(&r)
		if err != nil {
			return err
		}
	}
	return nil
}

func SetProc(path string, value string) error {
	return os.WriteFile(path, []byte(value), 0o644)
}

// CreateRulesOnDPUNode initializes the routing, firewall and ipset rules on the node.
// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh
func (s *Server) CreateRulesOnDPUNode(ztunnelVeth, ztunnelIP string, captureDNS bool) error {
	var err error

	log.Debugf("CreateRulesOnNode: ztunnelVeth=%s, ztunnelIP=%s", ztunnelVeth, ztunnelIP)

	// Check if chain exists, if it exists flush.. otherwise initialize
	// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L28
	err = execute(IptablesCmd, "-t", "mangle", "-C", "output", "-j", constants.ChainZTunnelOutput)
	if err == nil {
		log.Debugf("Chain %s already exists, flushing", constants.ChainOutput)
		s.flushLists()
	} else {
		log.Debugf("Initializing lists")
		err = s.initializeLists()
		if err != nil {
			return err
		}
	}

	// Create ipset of pod members.
	// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L85
	log.Debug("Creating ipset")
	err = Ipset.CreateSet()
	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("error creating ipset: %v", err)
	}

	appendRules := []*iptablesRule{
		// Skip things that come from the tunnels, but don't apply the conn skip mark
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L88
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", constants.InboundTun,
			"-j", "MARK",
			"--set-mark", constants.SkipMark,
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L89
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", constants.InboundTun,
			"-j", "RETURN",
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L90
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", constants.OutboundTun,
			"-j", "MARK",
			"--set-mark", constants.SkipMark,
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L91
		newIptableRule(constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", constants.OutboundTun,
			"-j", "RETURN",
		),

		// Make sure that whatever is skipped is also skipped for returning packets.
		// If we have a skip mark, save it to conn mark.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L95
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelForward,
			"-m", "mark",
			"--mark", constants.ConnSkipMark,
			"-j", "CONNMARK",
			"--save-mark",
			"--nfmask", constants.ConnSkipMask,
			"--ctmask", constants.ConnSkipMask,
		),
		// Input chain might be needed for things in host namespace that are skipped.
		// Place the mark here after routing was done, not sure if conn-tracking will figure
		// it out if I do it before, as NAT might change the connection tuple.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L99
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelInput,
			"-m", "mark",
			"--mark", constants.ConnSkipMark,
			"-j", "CONNMARK",
			"--save-mark",
			"--nfmask", constants.ConnSkipMask,
			"--ctmask", constants.ConnSkipMask,
		),

		// For things with the proxy mark, we need different routing just on returning packets
		// so we give a different mark to them.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L103
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelForward,
			"-m", "mark",
			"--mark", constants.ProxyMark,
			"-j", "CONNMARK",
			"--save-mark",
			"--nfmask", constants.ProxyMask,
			"--ctmask", constants.ProxyMask,
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L104
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelInput,
			"-m", "mark",
			"--mark", constants.ProxyMark,
			"-j", "CONNMARK",
			"--save-mark",
			"--nfmask", constants.ProxyMask,
			"--ctmask", constants.ProxyMask,
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L106
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelOutput,
			"--source", HostIP,
			"-j", "MARK",
			"--set-mark", constants.ConnSkipMask,
		),

		// If we have an outbound mark, we don't need kube-proxy to do anything,
		// so accept it before kube-proxy translates service vips to pod ips
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L122
		newIptableRule(
			constants.TableNat,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.OutboundMark,
			"-j", "ACCEPT",
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L123
		newIptableRule(
			constants.TableNat,
			constants.ChainZTunnelPostrouting,
			"-m", "mark",
			"--mark", constants.OutboundMark,
			"-j", "ACCEPT",
		),
	}

	if captureDNS {
		appendRules = append(appendRules,
			newIptableRule(
				constants.TableNat,
				constants.ChainZTunnelPrerouting,
				"-p", "udp",
				"-m", "set",
				"--match-set", Ipset.Name, "src",
				"--dport", "53",
				"-j", "DNAT",
				"--to", fmt.Sprintf("%s:%d", ztunnelIP, constants.DNSCapturePort),
			),
		)
	}

	appendRules2 := []*iptablesRule{
		// Don't set anything on the tunnel (geneve port is 6081), as the tunnel copies
		// the mark to the un-tunneled packet.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L126
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-p", "udp",
			"-m", "udp",
			"--dport", "6081",
			"-j", "RETURN",
		),

		// If we have the conn mark, restore it to mark, to make sure that the other side of the connection
		// is skipped as well.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L129-L130
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "connmark",
			"--mark", constants.ConnSkipMark,
			"-j", "MARK",
			"--set-mark", constants.SkipMark,
		),
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.SkipMark,
			"-j", "RETURN",
		),

		// If we have the proxy mark in, set the return mark to make sure that original src packets go to ztunnel
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L133-L134
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"!", "-i", ztunnelVeth,
			"-m", "connmark",
			"--mark", constants.ProxyMark,
			"-j", "MARK",
			"--set-mark", constants.ProxyRetMark,
		),
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.ProxyRetMark,
			"-j", "RETURN",
		),

		// Send fake source outbound connections to the outbound route table (for original src)
		// if it's original src, the source ip of packets coming from the proxy might be that of a pod, so
		// make sure we don't tproxy it
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L139-L140
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", ztunnelVeth,
			"!", "--source", ztunnelIP,
			"-j", "MARK",
			"--set-mark", constants.ProxyMark,
		),
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.SkipMark,
			"-j", "RETURN",
		),

		// Make sure anything that leaves ztunnel is routed normally (xds, connections to other ztunnels,
		// connections to upstream pods...)
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L143
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-i", ztunnelVeth,
			"-j", "MARK",
			"--set-mark", constants.ConnSkipMark,
		),

		// skip udp so DNS works. We can make this more granular.
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L146
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-p", "udp",
			"-j", "MARK",
			"--set-mark", constants.ConnSkipMark,
		),

		// Skip things from host ip - these are usually kubectl probes
		// skip anything with skip mark. This can be used to add features like port exclusions
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L149
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-m", "mark",
			"--mark", constants.SkipMark,
			"-j", "RETURN",
		),

		// Mark outbound connections to route them to the proxy using ip rules/route tables
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L151
		// Per Yuval, interface_prefix can be left off this rule... but we should check this (hard to automate
		// detection).
		newIptableRule(
			constants.TableMangle,
			constants.ChainZTunnelPrerouting,
			"-p", "tcp",
			"-m", "set",
			"--match-set", Ipset.Name, "src",
			"-j", "MARK",
			"--set-mark", constants.OutboundMark,
		),
	}

	err = iptablesAppend(appendRules)
	if err != nil {
		log.Errorf("failed to append iptables rule: %v", err)
	}

	err = iptablesAppend(appendRules2)
	if err != nil {
		log.Errorf("failed to append iptables rule: %v", err)
	}

	// Need to do some work in procfs
	// @TODO: This likely needs to be cleaned up, there are a lot of martians in AWS
	// that seem to necessitate this work.
	procs := map[string]int{
		"/proc/sys/net/ipv4/conf/default/rp_filter":                0,
		"/proc/sys/net/ipv4/conf/all/rp_filter":                    0,
		"/proc/sys/net/ipv4/conf/" + ztunnelVeth + "/rp_filter":    0,
		"/proc/sys/net/ipv4/conf/" + ztunnelVeth + "/accept_local": 1,
	}
	for proc, val := range procs {
		err = SetProc(proc, fmt.Sprint(val))
		if err != nil {
			log.Errorf("failed to write to proc file %s: %v", proc, err)
		}
	}

	// Create tunnels
	// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L153-L161
	inbnd := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: constants.InboundTun,
		},
		ID:     1000,
		Remote: net.ParseIP(ztunnelIP),
	}
	log.Debugf("Building inbound tunnel: %+v", inbnd)
	err = netlink.LinkAdd(inbnd)
	if err != nil {
		log.Errorf("failed to add inbound tunnel: %v", err)
	}
	err = netlink.AddrAdd(inbnd, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   net.ParseIP(constants.InboundTunIP),
			Mask: net.CIDRMask(constants.TunPrefix, 32),
		},
	})
	if err != nil {
		log.Errorf("failed to add inbound tunnel address: %v", err)
	}

	outbnd := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: constants.OutboundTun,
		},
		ID:     1001,
		Remote: net.ParseIP(ztunnelIP),
	}
	log.Debugf("Building outbound tunnel: %+v", outbnd)
	err = netlink.LinkAdd(outbnd)
	if err != nil {
		log.Errorf("failed to add outbound tunnel: %v", err)
	}
	err = netlink.AddrAdd(outbnd, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   net.ParseIP(constants.OutboundTunIP),
			Mask: net.CIDRMask(constants.TunPrefix, 32),
		},
	})
	if err != nil {
		log.Errorf("failed to add outbound tunnel address: %v", err)
	}

	cputun := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: constants.CPUTun,
		},
		ID:     1000,
		Remote: net.ParseIP(offmesh.GetPair(NodeName, offmesh.DPUNode, s.offmeshCluster).IP),
	}
	log.Debugf("Building cpu tunnel: %+v", cputun)
	err = netlink.LinkAdd(cputun)
	if err != nil {
		log.Errorf("failed to add cpu tunnel: %v", err)
	}
	err = netlink.AddrAdd(cputun, &netlink.Addr{
		IPNet: &net.IPNet{
			IP:   net.ParseIP(constants.DPUCPUTunIP),
			Mask: net.CIDRMask(constants.TunPrefix, 32),
		},
	})
	if err != nil {
		log.Errorf("failed to add dpu tunnel address: %v", err)
	}

	err = netlink.LinkSetUp(inbnd)
	if err != nil {
		log.Errorf("failed to set inbound tunnel up: %v", err)
	}
	err = netlink.LinkSetUp(outbnd)
	if err != nil {
		log.Errorf("failed to set outbound tunnel up: %v", err)
	}
	err = netlink.LinkSetUp(cputun)
	if err != nil {
		log.Errorf("failed to set dpu tunnel up: %v", err)
	}

	procs = map[string]int{
		"/proc/sys/net/ipv4/conf/" + constants.InboundTun + "/rp_filter":     0,
		"/proc/sys/net/ipv4/conf/" + constants.InboundTun + "/accept_local":  1,
		"/proc/sys/net/ipv4/conf/" + constants.OutboundTun + "/rp_filter":    0,
		"/proc/sys/net/ipv4/conf/" + constants.OutboundTun + "/accept_local": 1,
	}
	for proc, val := range procs {
		err = SetProc(proc, fmt.Sprint(val))
		if err != nil {
			log.Errorf("failed to write to proc file %s: %v", proc, err)
		}
	}

	dirEntries, err := os.ReadDir("/proc/sys/net/ipv4/conf")
	if err != nil {
		log.Errorf("failed to read /proc/sys/net/ipv4/conf: %v", err)
	}
	for _, dirEntry := range dirEntries {
		if dirEntry.IsDir() {
			if _, err := os.Stat("/proc/sys/net/ipv4/conf/" + dirEntry.Name() + "/rp_filter"); err != nil {
				err := SetProc("/proc/sys/net/ipv4/conf/"+dirEntry.Name()+"/rp_filter", "0")
				if err != nil {
					log.Errorf("failed to set /proc/sys/net/ipv4/conf/%s/rp_filter: %v", dirEntry.Name(), err)
				}
			}
		}
	}

	routes := []*ExecList{
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L164
		newExec("ip",
			[]string{
				"route", "add", "table", fmt.Sprint(constants.RouteTableOutbound), ztunnelIP,
				"dev", ztunnelVeth, "scope", "link",
			},
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L166
		newExec("ip",
			[]string{
				"route", "add", "table", fmt.Sprint(constants.RouteTableOutbound), "0.0.0.0/0",
				"via", constants.ZTunnelOutboundTunIP, "dev", constants.OutboundTun,
			},
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L168
		newExec("ip",
			[]string{
				"route", "add", "table", fmt.Sprint(constants.RouteTableProxy), ztunnelIP,
				"dev", ztunnelVeth, "scope", "link",
			},
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L169
		newExec("ip",
			[]string{
				"route", "add", "table", fmt.Sprint(constants.RouteTableProxy), "0.0.0.0/0",
				"via", ztunnelIP, "dev", ztunnelVeth, "onlink",
			},
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L171
		newExec("ip",
			[]string{
				"route", "add", "table", fmt.Sprint(constants.RouteTableInbound), ztunnelIP,
				"dev", ztunnelVeth, "scope", "link",
			},
		),
		// https://github.com/solo-io/istio-sidecarless/blob/master/redirect-worker.sh#L62-L77
		// Everything with the skip mark goes directly to the main table
		newExec("ip",
			[]string{
				"rule", "add", "priority", "100",
				"fwmark", fmt.Sprint(constants.SkipMark),
				"goto", "32766",
			},
		),
		// Everything with the outbound mark goes to the tunnel out device
		// using the outbound route table
		newExec("ip",
			[]string{
				"rule", "add", "priority", "101",
				"fwmark", fmt.Sprint(constants.OutboundMark),
				"lookup", fmt.Sprint(constants.RouteTableOutbound),
			},
		),
		// Things with the proxy return mark go directly to the proxy veth using the proxy
		// route table (useful for original src)
		newExec("ip",
			[]string{
				"rule", "add", "priority", "102",
				"fwmark", fmt.Sprint(constants.ProxyRetMark),
				"lookup", fmt.Sprint(constants.RouteTableProxy),
			},
		),
		// Send all traffic to the inbound table. This table has routes only to pods in the mesh.
		// It does not have a catch-all route, so if a route is missing, the search will continue
		// allowing us to override routing just for member pods.
		newExec("ip",
			[]string{
				"rule", "add", "priority", "103",
				"table", fmt.Sprint(constants.RouteTableInbound),
			},
		),
	}

	for _, route := range routes {
		err = execute(route.Cmd, route.Args...)
		if err != nil {
			log.Errorf(fmt.Errorf("failed to add route (%+v): %v", route, err))
		}
	}

	return nil
}
