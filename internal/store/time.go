package store

import "time"

// unixTime is the storage boundary for required second-resolution timestamps.
func unixTime(seconds int64) time.Time {
	return time.Unix(seconds, 0).UTC()
}

// optionalUnixTime maps SQL's zero sentinel to the domain's zero time. The
// persistence model uses both NULL and zero for historical optional columns;
// neither representation is allowed to escape the store as an epoch date.
func optionalUnixTime(seconds int64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return unixTime(seconds)
}
