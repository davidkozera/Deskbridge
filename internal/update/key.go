package update

// PublicKey checks the signature on every update. The matching private key
// is not in this repository: it is the GitHub repository secret
// UPDATE_SIGNING_KEY, used only by the release workflow. To replace the
// pair, run "go run ./tools/sign -genkey".
const PublicKey = "sgrow3Gxc2hVIt2pG7kKVYTzJdWtBPIbFPcC/9UUJnI="
