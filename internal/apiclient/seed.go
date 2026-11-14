package apiclient

import (
	"github.com/ehharvey/homelab-ops/internal/hostos"
	"github.com/ehharvey/homelab-ops/internal/seedwire"
)

// DecodeSeed turns Seed's answer back into the files the instance's install
// media carries, through internal/seedwire's field-to-file table, the one the
// server encodes with. Every client-side media path reads a downloaded seed
// through it, so it's the one place a seed is decoded: when seed downloads
// are encrypted (#198), decrypting in memory goes here, ahead of the JSON.
func DecodeSeed(body []byte) ([]hostos.File, error) {
	return seedwire.Decode(body)
}
