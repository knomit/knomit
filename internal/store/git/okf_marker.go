package git

// OKF generation markers record, per source branch, the inputs that produced
// the current okf/<branch> ref. The store layer packs (source SHA, mapper
// version, OKF SHA) into value; git treats it as opaque.

func okfMarkerKey(branch string) string { return "okf:marker:" + branch }

// OKFMarkerGet returns the stored marker value for branch, or "" if unset.
func (s *Storer) OKFMarkerGet(branch string) (string, error) {
	v, err := s.kvGet(okfMarkerKey(branch))
	if err != nil {
		return "", err
	}
	return string(v), nil
}

// OKFMarkerSet stores the marker value for branch.
func (s *Storer) OKFMarkerSet(branch, value string) error {
	return s.kvSet(okfMarkerKey(branch), []byte(value))
}
