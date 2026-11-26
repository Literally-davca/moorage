package updater

import (
	"context"

	"moorage/internal/notify"
	"moorage/internal/store"
)

// hostName is the Moorage name of a host for notifications ("" when unknown).
func (u *Updater) hostName(id string) string {
	if u.reg == nil || id == "" {
		return ""
	}
	if _, e, _ := u.reg.Get(context.Background(), id); e.Name != "" {
		return e.Name
	}
	return ""
}

// imageChanges lists the images an update replaced (the Detailed preset shows them):
// "<ref> @sha256:<old>" → "@sha256:<new>".
func imageChanges(rec store.ImageUpdate) []notify.ImageChange {
	var out []notify.ImageChange
	for _, s := range rec.Services {
		if (s.OldID != "" && s.OldID == s.NewID) || (s.OldDigest != "" && s.OldDigest == s.NewDigest) {
			continue
		}
		from, to := s.Ref, s.Ref
		if s.OldDigest != "" {
			from += " (sha256:" + ShortDigest(s.OldDigest) + ")"
		}
		if s.NewDigest != "" {
			to += " (sha256:" + ShortDigest(s.NewDigest) + ")"
		}
		out = append(out, notify.ImageChange{Service: s.Service, From: from, To: to})
	}
	return out
}
