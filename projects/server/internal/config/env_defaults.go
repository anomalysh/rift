package config

import (
	"strconv"
)

// EnvDefault is one operator-relevant environment variable and its built-in
// default, as a string ready to write into a .env or an nftables template.
type EnvDefault struct {
	Key   string
	Value string
}

// EnvDefaults returns the defaults that operator tooling needs to stay in step
// with the server -- chiefly the raw-tunnel port window and the ingress bounds,
// which tools/harden.sh must open and tools/setup.sh documents. Sourcing these
// from `riftd config defaults` keeps a single source of truth: the Default*
// constants below cannot silently diverge from what a hand-maintained shell copy
// believes. Ordered for stable output.
func EnvDefaults() []EnvDefault {
	return []EnvDefault{
		{KeyTCPEnabled, strconv.FormatBool(DefaultTCPEnabled)},
		{KeyTCPListenHost, DefaultTCPListenHost},
		{KeyTCPPortMin, strconv.Itoa(DefaultTCPPortMin)},
		{KeyTCPPortMax, strconv.Itoa(DefaultTCPPortMax)},
		{KeyUDPEnabled, strconv.FormatBool(DefaultUDPEnabled)},
		{KeyUDPListenHost, DefaultUDPListenHost},
		{KeyUDPPortMin, strconv.Itoa(DefaultUDPPortMin)},
		{KeyUDPPortMax, strconv.Itoa(DefaultUDPPortMax)},
		{KeyTLSTunnelEnabled, strconv.FormatBool(DefaultTLSTunnelEnabled)},
		{KeyTLSTunnelListenAddr, DefaultTLSTunnelListenAddr},
		{KeyGRPCEnabled, strconv.FormatBool(DefaultGRPCEnabled)},
		{KeyGRPCListenAddr, DefaultGRPCListenAddr},
		{KeyIngressReadTimeout, DefaultIngressReadTimeout.String()},
		{KeyIngressMaxHeaderBytes, strconv.Itoa(DefaultIngressMaxHeaderBytes)},
		{KeyMaxRequestBodyBytes, strconv.FormatInt(DefaultMaxRequestBodyBytes, 10)},
	}
}
