package apitypes

import (
	"encoding/base32"
	"strconv"
	"strings"
)

// RuntimeControllerName names the actual process-local native controller.
// It is a relation in Metrics, not an independently indexed resource.
type RuntimeControllerName struct {
	home     HomeName
	identity [32]byte
}
type RuntimeRunName struct {
	controller RuntimeControllerName
	run        uint64
}

var observationIdentityEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func RuntimeControllerNameForIdentity(home HomeName, identity [32]byte) (RuntimeControllerName, error) {
	if !home.Valid() || ([16]byte(identity[:16]) == [16]byte{} || [16]byte(identity[16:]) == [16]byte{}) {
		return RuntimeControllerName{}, ErrResourceName
	}
	return RuntimeControllerName{home, identity}, nil
}
func (n RuntimeControllerName) String() string {
	if !n.home.Valid() {
		return ""
	}
	return n.home.String() + "/runtimeControllers/c-" + observationIdentityEncoding.EncodeToString(n.identity[:])
}
func (n RuntimeControllerName) Home() HomeName     { return n.home }
func (n RuntimeControllerName) Identity() [32]byte { return n.identity }
func ParseRuntimeControllerName(value string) (RuntimeControllerName, error) {
	if resourceName(value, "homes", "runtimeControllers") != nil {
		return RuntimeControllerName{}, ErrResourceName
	}
	homeText, id, ok := strings.Cut(value, "/runtimeControllers/c-")
	if !ok {
		return RuntimeControllerName{}, ErrResourceName
	}
	home, err := ParseHomeName(homeText)
	if err != nil {
		return RuntimeControllerName{}, err
	}
	raw, err := observationIdentityEncoding.DecodeString(id)
	if err != nil || len(raw) != 32 {
		return RuntimeControllerName{}, ErrResourceName
	}
	var identity [32]byte
	copy(identity[:], raw)
	name, err := RuntimeControllerNameForIdentity(home, identity)
	if err != nil || name.String() != value {
		return RuntimeControllerName{}, ErrResourceName
	}
	return name, nil
}
func RuntimeRunNameForIdentity(controller RuntimeControllerName, run uint64) (RuntimeRunName, error) {
	if controller.String() == "" || run == 0 {
		return RuntimeRunName{}, ErrResourceName
	}
	return RuntimeRunName{controller, run}, nil
}
func (n RuntimeRunName) String() string {
	if n.run == 0 {
		return ""
	}
	return n.controller.String() + "/runs/r-" + strconv.FormatUint(n.run, 36)
}
func (n RuntimeRunName) Controller() RuntimeControllerName { return n.controller }
func (n RuntimeRunName) Run() uint64                       { return n.run }
func ParseRuntimeRunName(value string) (RuntimeRunName, error) {
	if resourceName(value, "homes", "runtimeControllers", "runs") != nil {
		return RuntimeRunName{}, ErrResourceName
	}
	controllerText, id, ok := strings.Cut(value, "/runs/r-")
	if !ok {
		return RuntimeRunName{}, ErrResourceName
	}
	controller, err := ParseRuntimeControllerName(controllerText)
	if err != nil {
		return RuntimeRunName{}, err
	}
	run, err := strconv.ParseUint(id, 36, 64)
	if err != nil {
		return RuntimeRunName{}, ErrResourceName
	}
	name, err := RuntimeRunNameForIdentity(controller, run)
	if err != nil || name.String() != value {
		return RuntimeRunName{}, ErrResourceName
	}
	return name, nil
}
