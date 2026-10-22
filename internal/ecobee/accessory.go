package ecobee

import (
	"fmt"

	"github.com/mctofu/homekit/client"
	"github.com/mctofu/homekit/client/characteristic"
)

// Do not call upstream MustString readers on untrusted discovery metadata.
func admitECB601(accessories []*client.RawAccessory) error {
	for _, accessory := range accessories {
		if accessory == nil {
			return ErrSettings
		}
		thermostat := false
		for _, service := range accessory.Services {
			if service == nil {
				return ErrSettings
			}
			if service.Type == "4A" {
				thermostat = true
			}
		}
		if !thermostat {
			continue
		}
		found := false
		for _, service := range accessory.Services {
			if service.Type != "3E" {
				continue
			}
			for _, field := range service.Characteristics {
				if field == nil {
					return ErrSettings
				}
				if field.Type != characteristic.TypeModel {
					continue
				}
				model, err := field.Value.String()
				if err != nil || model != "ECB601" || found {
					return fmt.Errorf("ecobee: authenticated accessory is outside ECB601 model profile")
				}
				found = true
			}
		}
		if !found {
			return fmt.Errorf("ecobee: ECB601 model identity absent")
		}
	}
	return nil
}
