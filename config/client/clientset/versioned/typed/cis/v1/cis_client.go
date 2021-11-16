/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Code generated by client-gen. DO NOT EDIT.

package v1

import (
	v1 "github.com/F5Networks/k8s-bigip-ctlr/config/apis/cis/v1"
	"github.com/F5Networks/k8s-bigip-ctlr/config/client/clientset/versioned/scheme"
	rest "k8s.io/client-go/rest"
)

type CisV1Interface interface {
	RESTClient() rest.Interface
	ExternalDNSesGetter
	IngressLinksGetter
	PoliciesGetter
	TLSProfilesGetter
	TransportServersGetter
	VirtualServersGetter
}

// CisV1Client is used to interact with features provided by the cis.f5.com group.
type CisV1Client struct {
	restClient rest.Interface
}

func (c *CisV1Client) ExternalDNSes(namespace string) ExternalDNSInterface {
	return newExternalDNSes(c, namespace)
}

func (c *CisV1Client) IngressLinks(namespace string) IngressLinkInterface {
	return newIngressLinks(c, namespace)
}

func (c *CisV1Client) Policies(namespace string) PolicyInterface {
	return newPolicies(c, namespace)
}

func (c *CisV1Client) TLSProfiles(namespace string) TLSProfileInterface {
	return newTLSProfiles(c, namespace)
}

func (c *CisV1Client) TransportServers(namespace string) TransportServerInterface {
	return newTransportServers(c, namespace)
}

func (c *CisV1Client) VirtualServers(namespace string) VirtualServerInterface {
	return newVirtualServers(c, namespace)
}

// NewForConfig creates a new CisV1Client for the given config.
func NewForConfig(c *rest.Config) (*CisV1Client, error) {
	config := *c
	if err := setConfigDefaults(&config); err != nil {
		return nil, err
	}
	client, err := rest.RESTClientFor(&config)
	if err != nil {
		return nil, err
	}
	return &CisV1Client{client}, nil
}

// NewForConfigOrDie creates a new CisV1Client for the given config and
// panics if there is an error in the config.
func NewForConfigOrDie(c *rest.Config) *CisV1Client {
	client, err := NewForConfig(c)
	if err != nil {
		panic(err)
	}
	return client
}

// New creates a new CisV1Client for the given RESTClient.
func New(c rest.Interface) *CisV1Client {
	return &CisV1Client{c}
}

func setConfigDefaults(config *rest.Config) error {
	gv := v1.SchemeGroupVersion
	config.GroupVersion = &gv
	config.APIPath = "/apis"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	if config.UserAgent == "" {
		config.UserAgent = rest.DefaultKubernetesUserAgent()
	}

	return nil
}

// RESTClient returns a RESTClient that is used to communicate
// with API server by this client implementation.
func (c *CisV1Client) RESTClient() rest.Interface {
	if c == nil {
		return nil
	}
	return c.restClient
}
