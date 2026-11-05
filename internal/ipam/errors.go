package ipam

import "errors"

// ErrOutOfRange is returned when a static_ip is outside its network's CIDR
// or, if there is one, outside the range its nodes draw from: the static
// range, or dhcp.pool in full mode.
var ErrOutOfRange = errors.New("ipam: static_ip out of range")

// ErrDuplicate is returned when two instances on the same network resolve
// to the same static_ip.
var ErrDuplicate = errors.New("ipam: duplicate static_ip")

// ErrPoolExhausted is returned when a network has no free address left to
// auto-assign in the effective range its nodes draw from.
var ErrPoolExhausted = errors.New("ipam: pool exhausted")

// ErrUnassigned is returned by Reservations for an Instance with a mac but
// no assigned address: its input must be post-Assign.
var ErrUnassigned = errors.New("ipam: instance has no assigned address")
