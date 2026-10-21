package nha047

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"

	"github.com/damione1/Homeward/compat/ha/nha045"
)

type demandVariant struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

func demandVariants() []demandVariant {
	var result []demandVariant
	for _, entry := range []struct {
		kind DemandKind
		name string
	}{{LightOff, "LightOff"}, {LightOn, "LightOn"}, {SwitchOff, "SwitchOff"}, {SwitchOn, "SwitchOn"}, {FanOff, "FanOff"}, {FanOn, "FanOn"}, {ClimateTemperature, "ClimateTemperature"}, {ClimateMode, "ClimateMode"}, {ClimateHumidity, "ClimateHumidity"}, {ClimateFanMode, "ClimateFanMode"}, {ClimatePresetMode, "ClimatePresetMode"}, {Refusal, "Refusal"}} {
		result = append(result, demandVariant{entry.name, entry.kind.String()})
	}
	return result
}

func qualificationHASources() ([]SourceHash, error) {
	// Keep045's delivered pins authoritative instead of copying their table.
	manifest, err := nha045.Manifest()
	if err != nil {
		return nil, err
	}
	var shared struct {
		Sources []SourceHash `json:"sources"`
	}
	if err = json.Unmarshal(manifest, &shared); err != nil {
		return nil, err
	}
	// Verified from root's pinned042 image extraction, not a moving branch.
	return append(shared.Sources,
		SourceHash{"homeassistant.core", "1fe2fcfb91fbb6edfb4450dad3ae5ebc4e325664770fedbd607d103d91ce4f19"},
		SourceHash{"homeassistant.components.homeassistant.triggers.time", "2fa85a64abea11344fe71a2e371e72311973913ee7b0dd6a19419b541e558abc"},
		SourceHash{"homeassistant.util.yaml", "5b3a8dbf4e5a5d9c7aeef3609895fd289665bede4b722cd423c809b0e65000e3"},
		SourceHash{"homeassistant.util.yaml.loader", "9e42258d1e755e1a4a91b38086292cb0aa399d0a520f802a12bbfa2a3ce84685"},
		SourceHash{"homeassistant.components.homeassistant.scene", "6ce56571b71234365a1c44d4039ed1d9c5f0fc7ccdbe4ef96b8125a9a7f08921"},
		SourceHash{"homeassistant.helpers.state", "1fac98fb623a9961e763537fc66ea88f65996e14d5c23dff43c12c67f00a14e0"},
		SourceHash{"homeassistant.helpers.condition", "2f30d1323153a729aeeecbe72075a3bb3f24597ec57510d03f68c48f9a342513"},
		SourceHash{"homeassistant.components.scene", "8f2bb94ddf2cc99818632157c02bb998e83896139e8a57eed52414b175e4ab42"},
		SourceHash{"homeassistant.components.sun", "c974bd4b74c03b23ae762a40ae0a427c168f4df5780f4d0d1ef33cd846ae031f"},
		SourceHash{"homeassistant.helpers.sun", "cd7947305ec2c01bd3b8b355d774429b93a2080e17ffb7f175053419fd1a3b3e"},
		SourceHash{"homeassistant.components.light.reproduce_state", "4425c8663975e38bc57d93fde0721feca7591da987e016ea161fed4cc25d651d"},
		SourceHash{"homeassistant.components.switch.reproduce_state", "40dc7618bdfbe017783f76c996ec66edd96b05a54d9dcb251273a7139950f96c"},
		SourceHash{"homeassistant.components.fan.reproduce_state", "78dd2b27cd88f1f17605c43460ceae5afe7873847875f628ceef3421c1563a9e"},
		SourceHash{"homeassistant.components.climate.reproduce_state", "723a57a38e5c4617d7ffaed2953694eddf13a81cdf064f8d1e53cb6fbd3ff66a"},
		SourceHash{"homeassistant.components.sun.trigger", "ad816be78a27adf7917fd45bcc9101b0b1ef0d3d07d92f356492c6f0e81acfc3"},
	), nil
}

func probeSources() ([]SourceHash, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, (128<<20)+1))
	if err != nil {
		return nil, err
	}
	if n > 128<<20 {
		return nil, ErrInvalidCapture
	}
	return []SourceHash{{Path: "probe", SHA256: hex.EncodeToString(hash.Sum(nil))}}, nil
}
