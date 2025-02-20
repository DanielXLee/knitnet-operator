/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package globalnet

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/bits"
	"net"

	"k8s.io/apimachinery/pkg/api/errors"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/tkestack/knitnet-operator/controllers/ensures/broker"
)

type GlobalnetInfo struct {
	GlobalnetEnabled     bool
	GlobalnetCidrRange   string
	GlobalnetClusterSize uint
	GlobalCidrInfo       map[string]*GlobalNetwork
}

type GlobalNetwork struct {
	GlobalCIDRs []string
	ClusterID   string
}

type GlobalCIDR struct {
	cidr              string
	net               *net.IPNet
	allocatedClusters []*CIDR
	allocatedCount    int
}

type CIDR struct {
	network *net.IPNet
	size    int
	lastIP  uint
}

type Config struct {
	ClusterCIDR             string
	ClusterID               string
	GlobalnetCIDR           string
	ServiceCIDR             string
	GlobalnetClusterSize    uint
	ClusterCIDRAutoDetected bool
	ServiceCIDRAutoDetected bool
}

var globalCidr = GlobalCIDR{allocatedCount: 0}

func isOverlappingCIDR(cidrList []string, cidr string) (bool, error) {
	_, newNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return false, err
	}
	for _, v := range cidrList {
		_, baseNet, err := net.ParseCIDR(v)
		if err != nil {
			return false, err
		}
		if baseNet.Contains(newNet.IP) || newNet.Contains(baseNet.IP) {
			return true, nil
		}
	}
	return false, nil
}

func NewCIDR(cidr string) (CIDR, error) {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return CIDR{}, fmt.Errorf("invalid cidr %q passed as input", cidr)
	}
	ones, total := network.Mask.Size()
	size := total - ones
	lastIP := LastIP(network)
	clusterCidr := CIDR{network: network, size: size, lastIP: lastIP}
	return clusterCidr, nil
}

func LastIP(network *net.IPNet) uint {
	ones, total := network.Mask.Size()
	clusterSize := uint(total - ones)
	firstIPInt := ipToUint(network.IP)
	lastIPUint := (firstIPInt + 1<<clusterSize) - 1
	return lastIPUint
}

func allocateByCidr(cidr string) (uint, error) {
	requestedIP, requestedNetwork, err := net.ParseCIDR(cidr)
	if err != nil || !globalCidr.net.Contains(requestedIP) {
		return 0, fmt.Errorf("%s not a valid subnet of %v", cidr, globalCidr.net)
	}

	var clusterCidr CIDR
	if clusterCidr, err = NewCIDR(cidr); err != nil {
		return 0, err
	}

	if !globalCidr.net.Contains(uintToIP(clusterCidr.lastIP)) {
		return 0, fmt.Errorf("%s not a valid subnet of %v", cidr, globalCidr.net)
	}
	for i := 0; i < globalCidr.allocatedCount; i++ {
		allocated := globalCidr.allocatedClusters[i]
		if allocated.network.Contains(requestedIP) {
			// subset of already allocated, try next
			return allocated.lastIP, fmt.Errorf("%s subset of already allocated globalCidr %v", cidr, allocated.network)
		}
		if requestedNetwork.Contains(allocated.network.IP) {
			// already allocated is subset of requested, no valid lastIP
			return clusterCidr.lastIP, fmt.Errorf("%s overlaps with already allocated globalCidr %s", cidr, allocated.network)
		}
	}
	globalCidr.allocatedClusters = append(globalCidr.allocatedClusters, &clusterCidr)
	globalCidr.allocatedCount++
	return 0, nil
}

func allocateByClusterSize(numSize uint) (string, error) {
	bitSize := bits.LeadingZeros(0) - bits.LeadingZeros(numSize-1)
	_, totalbits := globalCidr.net.Mask.Size()
	clusterPrefix := totalbits - bitSize
	mask := net.CIDRMask(clusterPrefix, totalbits)

	cidr := fmt.Sprintf("%s/%d", globalCidr.net.IP, clusterPrefix)

	last, err := allocateByCidr(cidr)
	if err != nil && last == 0 {
		return "", err
	}
	for err != nil {
		nextNet := net.IPNet{
			IP:   uintToIP(last + 1),
			Mask: mask,
		}
		cidr = nextNet.String()
		last, err = allocateByCidr(cidr)
		if err != nil && last == 0 {
			return "", fmt.Errorf("allocation not available")
		}
	}
	return cidr, nil
}

func AllocateGlobalCIDR(globalnetInfo *GlobalnetInfo) (string, error) {
	globalCidr = GlobalCIDR{allocatedCount: 0, cidr: globalnetInfo.GlobalnetCidrRange}
	_, network, err := net.ParseCIDR(globalCidr.cidr)
	if err != nil {
		return "", fmt.Errorf("invalid GlobalCIDR %s configured", globalCidr.cidr)
	}
	globalCidr.net = network
	for _, globalNetwork := range globalnetInfo.GlobalCidrInfo {
		for _, otherCluster := range globalNetwork.GlobalCIDRs {
			otherClusterCIDR, err := NewCIDR(otherCluster)
			if err != nil {
				return "", err
			}
			globalCidr.allocatedClusters = append(globalCidr.allocatedClusters, &otherClusterCIDR)
			globalCidr.allocatedCount++
		}
	}
	return allocateByClusterSize(globalnetInfo.GlobalnetClusterSize)
}

func ipToUint(ip net.IP) uint {
	intIP := ip
	if len(ip) == 16 {
		intIP = ip[12:16]
	}
	return uint(binary.BigEndian.Uint32(intIP))
}

func uintToIP(ip uint) net.IP {
	netIP := make(net.IP, 4)
	binary.BigEndian.PutUint32(netIP, uint32(ip))
	return netIP
}

func GetValidClusterSize(cidrRange string, clusterSize uint) (uint, error) {
	_, network, err := net.ParseCIDR(cidrRange)
	if err != nil {
		return 0, err
	}
	ones, totalbits := network.Mask.Size()
	availableSize := 1 << uint(totalbits-ones)
	userClusterSize := clusterSize
	clusterSize = nextPowerOf2(uint32(clusterSize))
	if clusterSize > uint(availableSize/2) {
		return 0, fmt.Errorf("cluster size %d, should be <= %d", userClusterSize, availableSize/2)
	}
	return clusterSize, nil
}

//Refer: https://graphics.stanford.edu/~seander/bithacks.html#RoundUpPowerOf2
func nextPowerOf2(n uint32) uint {
	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++
	return uint(n)
}

func CheckOverlappingCidrs(globalnetInfo *GlobalnetInfo, netconfig Config) error {
	var cidrlist []string
	var cidr string
	for k, v := range globalnetInfo.GlobalCidrInfo {
		cidrlist = v.GlobalCIDRs
		cidr = netconfig.GlobalnetCIDR
		overlap, err := isOverlappingCIDR(cidrlist, cidr)
		if err != nil {
			return fmt.Errorf("unable to validate overlapping CIDR: %s", err)
		}
		if overlap && k != netconfig.ClusterID {
			return fmt.Errorf("invalid CIDR %s overlaps with cluster %q", cidr, k)
		}
	}
	return nil
}

func isCIDRPreConfigured(clusterID string, globalNetworks map[string]*GlobalNetwork) bool {
	// GlobalCIDR is not pre-configured
	if globalNetworks[clusterID] == nil || globalNetworks[clusterID].GlobalCIDRs == nil || len(globalNetworks[clusterID].GlobalCIDRs) == 0 {
		return false
	}
	// GlobalCIDR is pre-configured
	return true
}

func ValidateGlobalnetConfiguration(globalnetInfo *GlobalnetInfo, netconfig Config) (string, error) {
	klog.Info("Validating Globalnet configurations")
	globalnetClusterSize := netconfig.GlobalnetClusterSize
	globalnetCIDR := netconfig.GlobalnetCIDR
	if globalnetInfo.GlobalnetEnabled && globalnetClusterSize != 0 && globalnetClusterSize != globalnetInfo.GlobalnetClusterSize {
		clusterSize, err := GetValidClusterSize(globalnetInfo.GlobalnetCidrRange, globalnetClusterSize)
		if err != nil || clusterSize == 0 {
			return "", fmt.Errorf("invalid globalnet-cluster-size %s", err)
		}
		globalnetInfo.GlobalnetClusterSize = clusterSize
	}

	if globalnetCIDR != "" && globalnetClusterSize != 0 {
		return "", fmt.Errorf("both globalnet-cluster-size and globalnet-cidr can't be specified. Specify either one")
	}

	if globalnetCIDR != "" {
		_, _, err := net.ParseCIDR(globalnetCIDR)
		if err != nil {
			return "", fmt.Errorf("specified globalnet-cidr is invalid: %s", err)
		}
	}

	if !globalnetInfo.GlobalnetEnabled {
		if globalnetCIDR != "" {
			klog.Info("Globalnet is not enabled on Broker. Ignoring specified globalnet-cidr")
			globalnetCIDR = ""
		} else if globalnetClusterSize != 0 {
			klog.Info("Globalnet is not enabled on Broker. Ignoring specified globalnet-cluster-size")
			globalnetInfo.GlobalnetClusterSize = 0
		}
	}
	return globalnetCIDR, nil
}

func GetGlobalNetworks(reader client.Reader, brokerNamespace string) (*GlobalnetInfo, *v1.ConfigMap, error) {
	configMap, err := broker.GetGlobalnetConfigMap(reader, brokerNamespace)
	if err != nil {
		return nil, nil, err
	}

	globalnetInfo := GlobalnetInfo{}
	err = json.Unmarshal([]byte(configMap.Data[broker.GlobalnetStatusKey]), &globalnetInfo.GlobalnetEnabled)
	if err != nil {
		klog.Errorf("error reading globalnetEnabled status: %v", err)
		return nil, nil, err
	}

	if globalnetInfo.GlobalnetEnabled {
		err = json.Unmarshal([]byte(configMap.Data[broker.GlobalnetClusterSize]), &globalnetInfo.GlobalnetClusterSize)
		if err != nil {
			klog.Errorf("error reading GlobalnetClusterSize: %v", err)
			return nil, nil, err
		}

		err = json.Unmarshal([]byte(configMap.Data[broker.GlobalnetCidrRange]), &globalnetInfo.GlobalnetCidrRange)
		if err != nil {
			klog.Errorf("error reading GlobalnetCidrRange: %v", err)
			return nil, nil, err
		}
	}

	var clusterInfo []broker.ClusterInfo
	err = json.Unmarshal([]byte(configMap.Data[broker.ClusterInfoKey]), &clusterInfo)
	if err != nil {
		klog.Errorf("error reading globalnet clusterInfo: %v", err)
		return nil, nil, err
	}

	var globalNetworks = make(map[string]*GlobalNetwork)
	for _, cluster := range clusterInfo {
		globalNetwork := GlobalNetwork{
			GlobalCIDRs: cluster.GlobalCidr,
			ClusterID:   cluster.ClusterID,
		}
		globalNetworks[cluster.ClusterID] = &globalNetwork
	}

	globalnetInfo.GlobalCidrInfo = globalNetworks
	return &globalnetInfo, configMap, nil
}

func AssignGlobalnetIPs(globalnetInfo *GlobalnetInfo, netconfig Config) (string, error) {
	klog.Info("Assigning Globalnet IPs")
	globalnetCIDR := netconfig.GlobalnetCIDR
	clusterID := netconfig.ClusterID
	var err error
	if globalnetCIDR == "" {
		// Globalnet enabled, GlobalCIDR not specified by the user
		if isCIDRPreConfigured(clusterID, globalnetInfo.GlobalCidrInfo) {
			// globalCidr already configured on this cluster
			globalnetCIDR = globalnetInfo.GlobalCidrInfo[clusterID].GlobalCIDRs[0]
			klog.Infof("Cluster already has GlobalCIDR allocated: %s", globalnetCIDR)
		} else {
			// no globalCidr configured on this cluster
			globalnetCIDR, err = AllocateGlobalCIDR(globalnetInfo)
			if err != nil {
				klog.Errorf("globalnet failed: %v", err)
				return "", err
			}
			klog.Infof("Allocated GlobalCIDR: %s", globalnetCIDR)
		}
	} else {
		// Globalnet enabled, globalnetCIDR specified by user
		if isCIDRPreConfigured(clusterID, globalnetInfo.GlobalCidrInfo) {
			// globalCidr pre-configured on this cluster
			globalnetCIDR = globalnetInfo.GlobalCidrInfo[clusterID].GlobalCIDRs[0]
			klog.Infof("Pre-configured GlobalCIDR %s detected. Not changing it.", globalnetCIDR)
		} else {
			// globalCidr as specified by the user
			err := CheckOverlappingCidrs(globalnetInfo, netconfig)
			if err != nil {
				klog.Errorf("error validating overlapping GlobalCIDRs %s: %v", globalnetCIDR, err)
				return "", err
			}
			klog.Infof("GlobalCIDR is: %s", globalnetCIDR)
		}
	}
	return globalnetCIDR, nil
}

func IsValidCIDR(cidr string) error {
	ip, _, err := net.ParseCIDR(cidr)

	if err != nil {
		return err
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("%s can't be unspecified", cidr)
	}
	if ip.IsLoopback() {
		return fmt.Errorf("%s can't be in loopback range", cidr)
	}
	if ip.IsLinkLocalUnicast() {
		return fmt.Errorf("%s can't be in link-local range", cidr)
	}
	if ip.IsLinkLocalMulticast() {
		return fmt.Errorf("%s can't be in link-local multicast range", cidr)
	}
	return nil
}

func ValidateExistingGlobalNetworks(reader client.Reader, namespace string) error {
	globalnetInfo, _, err := GetGlobalNetworks(reader, namespace)
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		klog.Errorf("error getting existing globalnet configmap: %v", err)
		return err
	}

	if globalnetInfo != nil {
		if err = IsValidCIDR(globalnetInfo.GlobalnetCidrRange); err != nil {
			klog.Errorf("invalid GlobalnetCidrRange: %v", err)
			return err
		}
	}
	return nil
}
