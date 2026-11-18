package aws

import (
	"encoding/json"
	"fmt"
	"os"
)

// addonResource maps an addon pin to the aws_eks_addon resource name the
// GlueOps EKS module gives it.
var addonResource = map[string]string{
	"csi_driver_version": "ebs_csi",
	"coredns_version":    "coredns",
	"kube_proxy_version": "kube_proxy",
}

// StateAddonVersions reads the installed addon versions from a local
// terraform.tfstate, keyed by addon pin. A missing file returns (nil, nil):
// captains with a remote backend have none.
func StateAddonVersions(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st struct {
		Resources []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				Attributes struct {
					AddonVersion string `json:"addon_version"`
				} `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]string{}
	for key, name := range addonResource {
		for _, r := range st.Resources {
			if r.Type == "aws_eks_addon" && r.Name == name && len(r.Instances) > 0 {
				out[key] = r.Instances[0].Attributes.AddonVersion
				break
			}
		}
	}
	return out, nil
}
