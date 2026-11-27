package auth

// RebuildForTest assembles a key from parts with a valid checksum, so
// external tests can craft keys that parse but are wrong in another way.
func RebuildForTest(env, prefix, secret string) string { return withChecksum(env, prefix, secret) }
