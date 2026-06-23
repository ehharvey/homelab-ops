package ipam

import "errors"

// ErrOutOfRange is returned when a static_ip is outside its network's CIDR
// or, if configured, outside dhcp_excluded_range.
var ErrOutOfRange = errors.New("ipam: static_ip out of range")

// ErrDuplicate is returned when two instances on the same network resolve
// to the same static_ip.
var ErrDuplicate = errors.New("ipam: duplicate static_ip")

// ErrPoolExhausted is returned when a network has no free address left in
// its usable static pool (dhcp_excluded_range) to auto-assign.
var ErrPoolExhausted = errors.New("ipam: pool exhausted")
