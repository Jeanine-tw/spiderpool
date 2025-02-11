// Copyright 2023 Authors of spidernet-io
// SPDX-License-Identifier: Apache-2.0

package networking

import (
	"fmt"
	"net"
	"os"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
)

var (
	defaultRulePriority = 1000
)

// GetRoutesByName return all routes is belonged to specify interface
// filter by family also
func GetRoutesByName(iface string, ipfamily int) (routes []netlink.Route, err error) {
	var link netlink.Link
	if iface != "" {
		link, err = netlink.LinkByName(iface)
		if err != nil {
			return nil, err
		}
	}

	return netlink.RouteList(link, ipfamily)
}

func GetDefaultGatewayByName(iface string, ipfamily int) ([]string, error) {
	routes, err := GetRoutesByName("", ipfamily)
	if err != nil {
		return nil, err
	}

	link, err := netlink.LinkByName(iface)
	if err != nil {
		return nil, err
	}

	gws := make([]string, 0)
	for _, route := range routes {
		if route.LinkIndex == link.Attrs().Index {
			if route.Dst == nil || route.Dst.IP.Equal(net.IPv4zero) {
				gws = append(gws, route.Gw.String())
			}
		} else {
			if len(route.MultiPath) > 0 {
				for _, r := range route.MultiPath {
					if r.LinkIndex == link.Attrs().Index {
						gws = append(gws, r.Gw.String())
						break
					}
				}
			}
		}
	}
	return gws, nil
}

func AddToRuleTable(dst *net.IPNet, ruleTable int) error {
	rule := netlink.NewRule()
	rule.Table = ruleTable
	rule.Dst = dst
	return netlink.RuleAdd(rule)
}

func DelToRuleTable(dst *net.IPNet, ruleTable int) error {
	rule := netlink.NewRule()
	rule.Table = ruleTable
	rule.Dst = dst
	return netlink.RuleDel(rule)
}

func AddRuleTableWithMark(mark, ruleTable, ipFamily int) error {
	rule := netlink.NewRule()
	rule.Mark = mark
	rule.Table = ruleTable
	rule.Family = ipFamily
	rule.Priority = defaultRulePriority
	return netlink.RuleAdd(rule)
}

// AddFromRuleTable add route rule for calico/cilium cidr(ipv4 and ipv6)
// Equivalent to: `ip rule add from <cidr> `
func AddFromRuleTable(src *net.IPNet, ruleTable int) error {
	rule := netlink.NewRule()
	rule.Table = ruleTable
	rule.Src = src
	return netlink.RuleAdd(rule)
}

// DelFromRuleTable equivalent to: `ip rule del from <cidr> lookup <ruletable>`
func DelFromRuleTable(src *net.IPNet, ruleTable int) error {
	rule := netlink.NewRule()
	rule.Table = ruleTable
	rule.Src = src
	return netlink.RuleDel(rule)
}

// AddRoute add static route to specify rule table
func AddRoute(logger *zap.Logger, ruleTable, ipFamily int, scope netlink.Scope, iface string, dst *net.IPNet, v4Gw, v6Gw net.IP) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	route := &netlink.Route{
		LinkIndex: link.Attrs().Index,
		Scope:     scope,
		Dst:       dst,
		Table:     ruleTable,
	}

	switch ipFamily {
	case netlink.FAMILY_V4:
		if v4Gw != nil {
			route.Gw = v4Gw
		}
	case netlink.FAMILY_V6:
		if v6Gw != nil {
			route.Gw = v6Gw
		}
	case netlink.FAMILY_ALL:
		if dst != nil && dst.IP.To4() != nil && v4Gw != nil {
			route.Gw = v4Gw
		}

		if dst != nil && dst.IP.To4() == nil && v6Gw != nil {
			route.Gw = v6Gw
		}
	default:
		return fmt.Errorf("unknown ipFamily %v", ipFamily)
	}

	if err = netlink.RouteAdd(route); err != nil && !os.IsExist(err) {
		logger.Error("failed to RouteAdd", zap.String("route", route.String()), zap.Error(err))
		return fmt.Errorf("failed to add route table(%v): %v", route.String(), err)
	}
	return nil
}

// MoveRouteTable move all routes of the specified interface to a new route table
// Equivalent: `ip route del <route>` and `ip r route add <route> <table>`
func MoveRouteTable(logger *zap.Logger, iface string, srcRuleTable, dstRuleTable, ipfamily int) error {
	logger.Debug("Debug MoveRouteTable", zap.String("interface", iface),
		zap.Int("srcRuleTable", srcRuleTable), zap.Int("dstRuleTable", dstRuleTable))
	link, err := netlink.LinkByName(iface)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	routes, err := netlink.RouteList(nil, ipfamily)
	if err != nil {
		logger.Error(err.Error())
		return err
	}

	for _, route := range routes {
		// only handle route tables from table main
		if route.Table != srcRuleTable {
			continue
		}

		// ignore local link route
		if route.Dst.String() == "fe80::/64" {
			continue
		}

		if route.LinkIndex == link.Attrs().Index {
			if err = netlink.RouteDel(&route); err != nil {
				logger.Error("failed to RouteDel in main", zap.String("route", route.String()), zap.Error(err))
				return fmt.Errorf("failed to RouteDel %s in main table: %+v", route.String(), err)
			}
			logger.Debug("Del the route from main successfully", zap.String("Route", route.String()))

			route.Table = dstRuleTable
			if err = netlink.RouteAdd(&route); err != nil && !os.IsExist(err) {
				logger.Error("failed to RouteAdd in new table ", zap.String("route", route.String()), zap.Error(err))
				return fmt.Errorf("failed to RouteAdd (%+v) to new table: %+v", route, err)
			}
			logger.Debug("MoveRoute to new table successfully", zap.String("Route", route.String()))
		} else {
			// especially for ipv6 default route
			if len(route.MultiPath) == 0 {
				continue
			}

			var generatedRoute, deletedRoute *netlink.Route
			// get generated default Route for new table
			for _, v := range route.MultiPath {
				logger.Debug("Found IPv6 Default Route", zap.String("Route", route.String()),
					zap.Int("v.LinkIndex", v.LinkIndex), zap.Int("link.Attrs().Index", link.Attrs().Index))
				if v.LinkIndex == link.Attrs().Index {
					generatedRoute = &netlink.Route{
						LinkIndex: v.LinkIndex,
						Gw:        v.Gw,
						Table:     dstRuleTable,
						MTU:       route.MTU,
					}
					deletedRoute = &netlink.Route{
						LinkIndex: v.LinkIndex,
						Gw:        v.Gw,
						Table:     srcRuleTable,
					}
					break
				}
			}
			if generatedRoute == nil {
				continue
			}

			logger.Debug("deletedRoute", zap.String("deletedRoute", deletedRoute.String()))
			if err := netlink.RouteDel(deletedRoute); err != nil {
				logger.Error("failed to RouteDel for IPv6", zap.String("Route", route.String()), zap.Error(err))
				return fmt.Errorf("failed to RouteDel %v for IPv6: %+v", route.String(), err)
			}

			if err = netlink.RouteAdd(generatedRoute); err != nil && !os.IsExist(err) {
				logger.Error("failed to RouteAdd for IPv6 to new table", zap.String("route", route.String()), zap.Error(err))
				return fmt.Errorf("failed to RouteAdd for IPv6 (%+v) to new table: %+v", route.String(), err)
			}
		}
	}
	return nil
}

// GetDefaultRouteInterface returns the name of the NIC where the default route is located
// if filterInterface not be empty, return first default route interface
// otherwise filter filterInterface
func GetDefaultRouteInterface(ipfamily int, filterInterface string, netns ns.NetNS) (string, error) {
	var defaultInterface string
	err := netns.Do(func(_ ns.NetNS) error {
		routes, err := netlink.RouteList(nil, ipfamily)
		if err != nil {
			return err
		}

		if ipfamily == netlink.FAMILY_V6 {
			for idx := range routes {
				if len(routes[idx].MultiPath) > 0 {
					// found v6 default route
					for _, v6DefaultRoute := range routes[idx].MultiPath {
						defaultInterface, err = getDefaultRouteIface(v6DefaultRoute.LinkIndex, filterInterface)
						if err != nil {
							return err
						}
						if defaultInterface != "" {
							return nil
						}
					}
				}
			}
			return nil
		}

		for idx := range routes {
			if routes[idx].Family == netlink.FAMILY_V4 {
				if routes[idx].Dst == nil || routes[idx].Dst.IP.Equal(net.IPv4zero) {
					// found default route
					defaultInterface, err = getDefaultRouteIface(routes[idx].LinkIndex, filterInterface)
					if err != nil {
						return err
					}
					if defaultInterface != "" {
						return nil
					}
				}
			}
		}
		return nil
	})

	return defaultInterface, err
}

func getDefaultRouteIface(linkIndex int, ignore string) (string, error) {
	link, err := netlink.LinkByIndex(linkIndex)
	if err != nil {
		return "", err
	}

	if ignore != "" && link.Attrs().Name == ignore {
		return "", nil
	}
	return link.Attrs().Name, nil
}

func ConvertMaxMaskIPNet(nip net.IP) *net.IPNet {
	mIPNet := &net.IPNet{
		IP: nip,
	}
	if nip.To4() != nil {
		mIPNet.Mask = net.CIDRMask(32, 32)
	} else {
		mIPNet.Mask = net.CIDRMask(128, 128)
	}
	return mIPNet
}
