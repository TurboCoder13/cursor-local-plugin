package provider

import (
	"crypto/sha512"
	"encoding/binary"
	"time"
)

// Two 8 MiB windows retain duplicate completion IDs for at least 15 minutes.
// Eight independent hash slices keep false positives rare at one million IDs.
type completionFilter struct {
	Windows [2][]byte
	Active  int
	Epoch   time.Time
}

func (f *completionFilter) seen(id string, now time.Time) bool {
	if f.Windows[0] == nil {
		f.Windows[0] = make([]byte, 8<<20)
		f.Windows[1] = make([]byte, 8<<20)
		f.Epoch = now
	}
	steps := int(now.Sub(f.Epoch) / (15 * time.Minute))
	if steps > 0 {
		if steps > 1 {
			clear(f.Windows[0])
			clear(f.Windows[1])
		} else {
			f.Active = 1 - f.Active
			clear(f.Windows[f.Active])
		}
		f.Epoch = f.Epoch.Add(time.Duration(steps) * 15 * time.Minute)
	}
	digest := sha512.Sum512([]byte(id))
	var positions [8]uint32
	for i := range positions {
		positions[i] = binary.BigEndian.Uint32(digest[i*8:]) & ((1 << 26) - 1)
	}
	duplicate := false
	for _, window := range f.Windows {
		match := true
		for _, bit := range positions {
			if window[bit/8]&(1<<(bit%8)) == 0 {
				match = false
				break
			}
		}
		duplicate = duplicate || match
	}
	if duplicate {
		return true
	}
	for _, bit := range positions {
		f.Windows[f.Active][bit/8] |= 1 << (bit % 8)
	}
	return duplicate
}
