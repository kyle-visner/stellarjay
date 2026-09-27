# Runtime secrets

Run `go run ./cmd/stellarjay-server init ./secrets` from the repository root.

This directory is ignored except for this file. Never commit `data_key`,
`auth.json`, or the plaintext tokens printed by the initializer.
