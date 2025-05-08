package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lxc/incus/v6/shared/subprocess"

	"github.com/lxc/incus-os/incus-osd/api"
	"github.com/lxc/incus-os/incus-osd/internal/seed"
)

// networkdConfigFile represents a given filename and its contents.
type networkdConfigFile struct {
	Name     string
	Contents string
}

// generateNetworkConfiguration clears any existing configuration from /run/systemd/network/ and generates
// new config files from the supplied NetworkConfig struct.
func generateNetworkConfiguration(_ context.Context, networkCfg *api.SystemNetworkConfig) error {
	// Remove any existing configuration.
	err := os.RemoveAll(SystemdNetworkConfigPath)
	if err != nil {
		return err
	}

	err = os.Mkdir(SystemdNetworkConfigPath, 0o755)
	if err != nil {
		return err
	}

	// Generate .link files.
	for _, cfg := range generateLinkFileContents(*networkCfg) {
		err := os.WriteFile(filepath.Join(SystemdNetworkConfigPath, cfg.Name), []byte(cfg.Contents), 0o644)
		if err != nil {
			return err
		}
	}

	// Generate .netdev files.
	for _, cfg := range generateNetdevFileContents(*networkCfg) {
		err := os.WriteFile(filepath.Join(SystemdNetworkConfigPath, cfg.Name), []byte(cfg.Contents), 0o644)
		if err != nil {
			return err
		}
	}

	// Generate .network files.
	for _, cfg := range generateNetworkFileContents(*networkCfg) {
		err := os.WriteFile(filepath.Join(SystemdNetworkConfigPath, cfg.Name), []byte(cfg.Contents), 0o644)
		if err != nil {
			return err
		}
	}

	// Generate systemd-timesyncd configuration if any timeservers are defined.
	ntpCfg := ""
	if networkCfg.NTP != nil {
		ntpCfg = generateTimesyncContents(*networkCfg.NTP)

		if ntpCfg != "" {
			err := os.WriteFile(SystemdTimesyncConfigFile, []byte(ntpCfg), 0o644)
			if err != nil {
				return err
			}
		}
	}

	// If there's no NTP configuration, remove the old config file that might exist.
	if networkCfg.NTP == nil || ntpCfg == "" {
		_ = os.Remove(SystemdTimesyncConfigFile)
	}

	return nil
}

// ApplyNetworkConfiguration instructs systemd-networkd to apply the supplied network configuration.
func ApplyNetworkConfiguration(ctx context.Context, networkCfg *api.SystemNetworkConfig, timeout time.Duration) error {
	if networkCfg == nil {
		return errors.New("no network configuration provided")
	}

	// Get hostname and domain from network config, if defined.
	hostname := ""
	if networkCfg.DNS != nil && networkCfg.DNS.Hostname != "" {
		hostname = networkCfg.DNS.Hostname
		if networkCfg.DNS.Domain != "" {
			hostname += "." + networkCfg.DNS.Domain
		}
	}

	// Apply the configured hostname, or reset back to default if not set.
	err := SetHostname(ctx, hostname)
	if err != nil {
		return err
	}

	// Set proxy environment variables, or clear existing ones if none are defined.
	err = UpdateEnvironment(networkCfg.Proxy)
	if err != nil {
		return err
	}

	err = generateNetworkConfiguration(ctx, networkCfg)
	if err != nil {
		return err
	}

	// At system start there's a small race between udev being fully started and
	// our reconfiguring of the network. Sleep for a couple seconds before triggering udev.
	time.Sleep(2 * time.Second)

	// Trigger udev rule update to pickup device names.
	_, err = subprocess.RunCommandContext(ctx, "udevadm", "trigger", "--action=add")
	if err != nil {
		return err
	}

	// Wait for udev to be done processing the events.
	_, err = subprocess.RunCommandContext(ctx, "udevadm", "settle")
	if err != nil {
		return err
	}

	// Restart networking after new config files have been generated.
	err = RestartUnit(ctx, "systemd-networkd")
	if err != nil {
		return err
	}

	// (Re)start NTP time synchronization. Since we might be overriding the default fallback NTP servers,
	// the service is disabled by default and only started once we have performed the network (re)configuration.
	err = RestartUnit(ctx, "systemd-timesyncd")
	if err != nil {
		return err
	}

	// Wait for the network to apply.
	return waitForNetworkRoutable(ctx, networkCfg, timeout, seed.NetworkSeedExists())
}

// waitForNetworkRoutable waits up to a provided timeout for configured network interfaces,
// bonds, and vlans to become routable.
func waitForNetworkRoutable(ctx context.Context, networkCfg *api.SystemNetworkConfig, timeout time.Duration, requireAllRoutable bool) error {
	isRoutable := func(name string) bool {
		output, err := subprocess.RunCommandContext(ctx, "networkctl", "status", name)
		if err != nil {
			return false
		}

		return strings.Contains(output, "State: routable")
	}

	endTime := time.Now().Add(timeout)

	for {
		if time.Now().After(endTime) {
			return errors.New("timed out waiting for network to become routable")
		}

		time.Sleep(500 * time.Millisecond)

		if len(networkCfg.Interfaces) > 0 {
			allInterfacesRoutable := true
			atLestOneInterfaceRoutable := false

			for _, i := range networkCfg.Interfaces {
				routable := isRoutable(i.Name)

				allInterfacesRoutable = allInterfacesRoutable && routable
				atLestOneInterfaceRoutable = atLestOneInterfaceRoutable || routable
			}

			if requireAllRoutable && !allInterfacesRoutable {
				continue
			}

			if !requireAllRoutable && !atLestOneInterfaceRoutable {
				continue
			}
		}

		if len(networkCfg.Bonds) > 0 {
			allBondsRoutable := true
			atLestOneBondRoutable := false

			for _, b := range networkCfg.Bonds {
				routable := isRoutable(b.Name)

				allBondsRoutable = allBondsRoutable && routable
				atLestOneBondRoutable = atLestOneBondRoutable || routable
			}

			if requireAllRoutable && !allBondsRoutable {
				continue
			}

			if !requireAllRoutable && !atLestOneBondRoutable {
				continue
			}
		}

		if len(networkCfg.VLANs) > 0 {
			allVLANsRoutable := true
			atLestOneVLANRoutable := false

			for _, v := range networkCfg.VLANs {
				routable := isRoutable(v.Name)

				allVLANsRoutable = allVLANsRoutable && routable
				atLestOneVLANRoutable = atLestOneVLANRoutable || routable
			}

			if requireAllRoutable && !allVLANsRoutable {
				continue
			}

			if !requireAllRoutable && !atLestOneVLANRoutable {
				continue
			}
		}

		return nil
	}
}

// generateLinkFileContents generates the contents of systemd.link files. Returns an array of ConfigFile structs.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.link.html
func generateLinkFileContents(networkCfg api.SystemNetworkConfig) []networkdConfigFile {
	ret := []networkdConfigFile{}

	for _, i := range networkCfg.Interfaces {
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(i.Hwaddr, ":", ""))
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("00-en%s.link", strippedHwaddr),
			Contents: fmt.Sprintf(`[Match]
PermanentMACAddress=%s

[Link]
NamePolicy=
Name=en%s
`, i.Hwaddr, strippedHwaddr),
		})
	}

	for _, b := range networkCfg.Bonds {
		for _, member := range b.Members {
			strippedHwaddr := strings.ToLower(strings.ReplaceAll(member, ":", ""))
			ret = append(ret, networkdConfigFile{
				Name: fmt.Sprintf("01-en%s.link", strippedHwaddr),
				Contents: fmt.Sprintf(`[Match]
PermanentMACAddress=%s

[Link]
NamePolicy=
Name=en%s
`, member, strippedHwaddr),
			})
		}
	}

	return ret
}

// generateNetdevFileContents generates the contents of systemd.netdev files. Returns an array of networkdConfigFile structs.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.netdev.html
func generateNetdevFileContents(networkCfg api.SystemNetworkConfig) []networkdConfigFile {
	ret := []networkdConfigFile{}

	// Create a bridge device for each interface.
	for _, i := range networkCfg.Interfaces {
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(i.Hwaddr, ":", ""))
		mtuString := ""
		if i.MTU != 0 {
			mtuString = fmt.Sprintf("MTUBytes=%d", i.MTU)
		}
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("10-br%s.netdev", strippedHwaddr),
			Contents: fmt.Sprintf(`[NetDev]
Name=%s
Kind=bridge
MACAddress=%s
%s

[Bridge]
VLANFiltering=true
`, i.Name, i.Hwaddr, mtuString),
		})
	}

	// Create bond and bridge devices for each bond.
	for _, b := range networkCfg.Bonds {
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(b.Hwaddr, ":", ""))
		mtuString := ""
		if b.MTU != 0 {
			mtuString = fmt.Sprintf("MTUBytes=%d", b.MTU)
		}

		// Bond.
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("11-bn%s.netdev", strippedHwaddr),
			Contents: fmt.Sprintf(`[NetDev]
Name=bn%s
Kind=bond
MACAddress=%s
%s

[Bond]
Mode=%s
`, strippedHwaddr, b.Hwaddr, mtuString, b.Mode),
		})

		// Bridge.
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("11-br%s.netdev", strippedHwaddr),
			Contents: fmt.Sprintf(`[NetDev]
Name=%s
Kind=bridge
MACAddress=%s
%s

[Bridge]
VLANFiltering=true
`, b.Name, b.Hwaddr, mtuString),
		})
	}

	// Create vlans.
	for _, v := range networkCfg.VLANs {
		mtuString := ""
		if v.MTU != 0 {
			mtuString = fmt.Sprintf("MTUBytes=%d", v.MTU)
		}
		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("12-vl%s.netdev", v.Name),
			Contents: fmt.Sprintf(`[NetDev]
Name=vl%s
Kind=vlan
%s

[VLAN]
Id=%d
`, v.Name, mtuString, v.ID),
		})
	}

	return ret
}

// generateNetworkFileContents generates the contents of systemd.network files. Returns an array of networkdConfigFile structs.
// https://www.freedesktop.org/software/systemd/man/latest/systemd.network.html
func generateNetworkFileContents(networkCfg api.SystemNetworkConfig) []networkdConfigFile {
	ret := []networkdConfigFile{}

	// Create networks for each interface.
	for _, i := range networkCfg.Interfaces {
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(i.Hwaddr, ":", ""))
		cfgString := fmt.Sprintf(`[Match]
Name=%s

[DHCP]
ClientIdentifier=mac
RouteMetric=100
UseMTU=true

[Network]
%s`, i.Name, generateNetworkSectionContents(networkCfg.DNS, networkCfg.NTP))

		cfgString += processAddresses(i.Addresses)

		if len(i.Routes) > 0 {
			cfgString += processRoutes(i.Routes)
		}

		if i.VLAN != 0 {
			cfgString += fmt.Sprintf(`
[BridgeVLAN]
VLAN=1-4094
PVID=%d
`, i.VLAN)
		}

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("20-%s.network", i.Name),
			Contents: cfgString,
		})

		ret = append(ret, networkdConfigFile{
			Name: fmt.Sprintf("20-br%s.network", strippedHwaddr),
			Contents: fmt.Sprintf(`[Match]
Name=en%s

[Network]
Bridge=%s
LLDP=%s
EmitLLDP=%s
`, strippedHwaddr, i.Name, strconv.FormatBool(i.LLDP), strconv.FormatBool(i.LLDP)),
		})
	}

	// Create networks for each bond and its member(s).
	for _, b := range networkCfg.Bonds {
		strippedHwaddr := strings.ToLower(strings.ReplaceAll(b.Hwaddr, ":", ""))

		// Bond.
		cfgString := fmt.Sprintf(`[Match]
Name=%s

[DHCP]
ClientIdentifier=mac
RouteMetric=100
UseMTU=true

[Network]
%s`, b.Name, generateNetworkSectionContents(networkCfg.DNS, networkCfg.NTP))

		cfgString += processAddresses(b.Addresses)

		if len(b.Routes) > 0 {
			cfgString += processRoutes(b.Routes)
		}

		if b.VLAN != 0 {
			cfgString += fmt.Sprintf(`
[BridgeVLAN]
VLAN=1-4094
PVID=%d
`, b.VLAN)
		}

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("21-%s.network", b.Name),
			Contents: cfgString,
		})

		// Bridge.
		cfgString = fmt.Sprintf(`[Match]
Name=bn%s

[Network]
Bridge=%s
`, strippedHwaddr, b.Name)

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("21-br%s.network", strippedHwaddr),
			Contents: cfgString,
		})

		// Bond members.
		for index, member := range b.Members {
			memberStrippedHwaddr := strings.ToLower(strings.ReplaceAll(member, ":", ""))

			ret = append(ret, networkdConfigFile{
				Name: fmt.Sprintf("21-bn%s-dev%d.network", strippedHwaddr, index),
				Contents: fmt.Sprintf(`[Match]
Name=en%s

[Network]
Bond=bn%s
LLDP=%s
EmitLLDP=%s
`, memberStrippedHwaddr, strippedHwaddr, strconv.FormatBool(b.LLDP), strconv.FormatBool(b.LLDP)),
			})
		}
	}

	// Create networks for each VLAN.
	for _, v := range networkCfg.VLANs {
		cfgString := fmt.Sprintf(`[Match]
Name=%s

[DHCP]
ClientIdentifier=mac
RouteMetric=100
UseMTU=true

[Network]
VLAN=vl%s
%s`, v.Parent, v.Name, generateNetworkSectionContents(networkCfg.DNS, networkCfg.NTP))

		cfgString += processAddresses(v.Addresses)

		if len(v.Routes) > 0 {
			cfgString += processRoutes(v.Routes)
		}

		ret = append(ret, networkdConfigFile{
			Name:     fmt.Sprintf("22-vl%s.network", v.Name),
			Contents: cfgString,
		})
	}

	return ret
}

func processAddresses(addresses []string) string {
	ret := ""
	if len(addresses) != 0 {
		ret += "LinkLocalAddressing=ipv6\n"
	} else {
		ret += "LinkLocalAddressing=no\n"
		ret += "ConfigureWithoutCarrier=yes\n"
	}

	hasDHCP4 := false
	hasDHCP6 := false
	acceptIPv6RA := false
	for _, addr := range addresses {
		switch addr {
		case "dhcp4":
			hasDHCP4 = true
		case "dhcp6":
			hasDHCP6 = true
		case "slaac":
			acceptIPv6RA = true

		default:
			ret += fmt.Sprintf("Address=%s\n", addr)
		}
	}

	if acceptIPv6RA {
		ret += "IPv6AcceptRA=true\n"
	} else {
		ret += "IPv6AcceptRA=false\n"
	}

	if hasDHCP4 && hasDHCP6 { //nolint:gocritic
		ret += "DHCP=yes\n"
	} else if hasDHCP4 {
		ret += "DHCP=ipv4\n"
	} else if hasDHCP6 {
		ret += "DHCP=ipv6\n"
	}

	return ret
}

func processRoutes(routes []api.SystemNetworkRoute) string {
	ret := "\n[Route]\n"

	for _, route := range routes {
		switch route.Via {
		case "dhcp4":
			ret += "Gateway=_dhcp4\n"
		case "slaac":
			ret += "Gateway=_ipv6ra\n"
		default:
			ret += fmt.Sprintf("Gateway=%s\n", route.Via)
		}

		ret += fmt.Sprintf("Destination=%s\n", route.To)
	}

	return ret
}

func generateNetworkSectionContents(dns *api.SystemNetworkDNS, ntp *api.SystemNetworkNTP) string {
	ret := ""

	// If there are search domains or name servers, add those to the config.
	if dns != nil {
		if len(dns.SearchDomains) > 0 {
			ret += fmt.Sprintf("Domains=%s\n", strings.Join(dns.SearchDomains, " "))
		}

		for _, ns := range dns.Nameservers {
			ret += fmt.Sprintf("DNS=%s\n", ns)
		}
	}

	// If there are time servers defined, add them to the config.
	if ntp != nil {
		for _, ts := range ntp.Timeservers {
			ret += fmt.Sprintf("NTP=%s\n", ts)
		}
	}

	return ret
}

func generateTimesyncContents(ntp api.SystemNetworkNTP) string {
	if len(ntp.Timeservers) == 0 {
		return ""
	}

	return "[Time]\nFallbackNTP=" + strings.Join(ntp.Timeservers, " ") + "\n"
}
