/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"github.com/ghodss/yaml"

	cmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	issuerRefKey                = "issuerRef"
	clusterInternalIssuerRefKey = "clusterInternalIssuerRef"

	// CertManagerConfigName is the name of the configmap containing all
	// configuration related to Cert-Manager.
	CertManagerConfigName = "config-certmanager"
)

// has to match the values in config/knative-cluster-issuer.yaml
var knativeInternalIssuer = &cmeta.ObjectReference{
	Kind: "ClusterIssuer",
	Name: "knative-internal-encryption-ca",
}

// CertManagerConfig contains Cert-Manager related configuration defined in the
// `config-certmanager` config map.
type CertManagerConfig struct {
	IssuerRef                *cmeta.ObjectReference
	ClusterInternalIssuerRef *cmeta.ObjectReference
}

// NewCertManagerConfigFromConfigMap creates an CertManagerConfig from the supplied ConfigMap
func NewCertManagerConfigFromConfigMap(configMap *corev1.ConfigMap) (*CertManagerConfig, error) {
	// Use Knative self-signed ClusterIssuer as default
	config := &CertManagerConfig{
		IssuerRef:                knativeInternalIssuer,
		ClusterInternalIssuerRef: knativeInternalIssuer,
	}

	if v, ok := configMap.Data[issuerRefKey]; ok {
		if err := yaml.Unmarshal([]byte(v), config.IssuerRef); err != nil {
			return nil, err
		}
	}

	if v, ok := configMap.Data[clusterInternalIssuerRefKey]; ok {
		if err := yaml.Unmarshal([]byte(v), config.ClusterInternalIssuerRef); err != nil {
			return nil, err
		}
	}

	return config, nil
}
