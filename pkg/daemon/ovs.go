package daemon

import (
	"fmt"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"net"
	"os/exec"
)

func (csc CniServerHandler) configureNic(podName, podNamespace, netns, containerID, mac, ip string) error {
	var err error
	hostNicName, containerNicName := generateNicName(containerID)

	// Create a veth pair, put one end to container ,the other to ovs port
	// NOTE: DO NOT use ovs internal type interface for container.
	// Kubernetes will detect 'eth0' nic in pod, so the nic name in pod must be 'eth0'.
	// When renaming internal interface to 'eth0', ovs will delete and recreate this interface.
	veth := netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: hostNicName}, PeerName: containerNicName}
	defer func() {
		// Remove veth link in case any error during creating pod network.
		if err != nil {
			netlink.LinkDel(&veth)
		}
	}()
	err = netlink.LinkAdd(&veth)
	if err != nil {
		return fmt.Errorf("failed to crate veth for %s %v", podName, err)
	}

	// Add veth pair host end to ovs port
	output, err := exec.Command("ovs-vsctl", "add-port", "br-int", hostNicName, "--", "set", "interface", hostNicName, fmt.Sprintf("external_ids:iface-id=%s.%s", podName, podNamespace)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("add nic to ovs failed %v: %s", err, output)
	}

	// host and container nic must use same mac address, otherwise ovn will reject these packets by default
	macAddr, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("failed to parse mac %s %v", macAddr, err)
	}

	err = configureHostNic(hostNicName, macAddr)
	if err != nil {
		return err
	}

	podNS, err := ns.GetNS(netns)
	if err != nil {
		return fmt.Errorf("failed to open netns %q: %v", netns, err)
	}
	err = configureContainerNic(containerNicName, ip, macAddr, podNS)
	if err != nil {
		return err
	}

	return nil
}

func (csc CniServerHandler) deleteNic(netns, containerID string) error {
	hostNicName, _ := generateNicName(containerID)
	// Remove ovs port
	output, err := exec.Command("ovs-vsctl", "--if-exists", "--with-iface", "del-port", "br-int", hostNicName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to delete ovs port %v, %s", err, output)
	}

	hostLink, err := netlink.LinkByName(hostNicName)
	if err != nil {
		// If link already not exists, return quietly
		if _, ok := err.(netlink.LinkNotFoundError); ok {
			return nil
		}
		return fmt.Errorf("find host link %s failed %v", hostNicName, err)
	}
	err = netlink.LinkDel(hostLink)
	if err != nil {
		return fmt.Errorf("delete host link %s failed %v", hostLink, err)
	}
	return nil
}

func generateNicName(containerID string) (string, string) {
	return fmt.Sprintf("%s_h", containerID[0:12]), fmt.Sprintf("%s_c", containerID[0:12])
}

func configureHostNic(nicName string, macAddr net.HardwareAddr) error {
	hostLink, err := netlink.LinkByName(nicName)
	if err != nil {
		return fmt.Errorf("can not find host nic %s %v", nicName, err)
	}

	err = netlink.LinkSetHardwareAddr(hostLink, macAddr)
	if err != nil {
		return fmt.Errorf("can not set mac address to host nic %s %v", nicName, err)
	}
	err = netlink.LinkSetUp(hostLink)
	if err != nil {
		return fmt.Errorf("can not set host nic %s up %v", nicName, err)
	}
	return nil
}

func configureContainerNic(nicName, ipAddr string, macaddr net.HardwareAddr, netns ns.NetNS) error {
	containerLink, err := netlink.LinkByName(nicName)
	if err != nil {
		return fmt.Errorf("can not find container nic %s %v", nicName, err)
	}

	err = netlink.LinkSetNsFd(containerLink, int(netns.Fd()))
	if err != nil {
		return fmt.Errorf("failed to link netns %v", err)
	}

	return ns.WithNetNSPath(netns.Path(), func(_ ns.NetNS) error {
		// Container nic name MUST be 'eth0', otherwise kubelet will recreate the pod
		err = netlink.LinkSetName(containerLink, "eth0")
		if err != nil {
			return err
		}
		addr, err := netlink.ParseAddr(ipAddr)
		if err != nil {
			return fmt.Errorf("can not parse %s %v", ipAddr, err)
		}
		err = netlink.AddrAdd(containerLink, addr)
		if err != nil {
			return fmt.Errorf("can not add address to container nic %v", err)
		}

		err = netlink.LinkSetHardwareAddr(containerLink, macaddr)
		if err != nil {
			return fmt.Errorf("can not set mac address to container nic %v", err)
		}
		err = netlink.LinkSetUp(containerLink)
		if err != nil {
			return fmt.Errorf("can not set container nic %s up %v", nicName, err)
		}
		return nil
	})
}
