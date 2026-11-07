package worker

// One current Host/driver contract. Historical workers are rejected before any
// capability grant, secret read, observation or command delivery.
const hostProtocolMajor uint32 = 1
const hostProtocolMinor uint32 = 5
