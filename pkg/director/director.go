package director

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"

	"github.com/evry-bergen/waf-syncer/pkg/config"
	"github.com/evry-bergen/waf-syncer/pkg/crypto"

	"github.com/Azure/go-autorest/autorest/to"

	istioApiv1alpha3 "github.com/knative/pkg/apis/istio/v1alpha3"

	istio "github.com/evry-bergen/waf-syncer/pkg/clients/istio/clientset/versioned"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/evry-bergen/waf-syncer/pkg/clients/istio/informers/externalversions/istio/v1alpha3"

	azureNetwork "github.com/Azure/azure-sdk-for-go/services/network/mgmt/2018-12-01/network"
	"k8s.io/client-go/kubernetes"

	"go.uber.org/zap"
	sslMate "software.sslmate.com/src/go-pkcs12"
)

// Director - struct for convenience
type Director struct {
	AzureWafConfig        *config.AzureWafConfig
	AzureAGClient         *azureNetwork.ApplicationGatewaysClient
	ClientSet             *kubernetes.Clientset
	IstioClient           *istio.Clientset
	GatewayInformer       v1alpha3.GatewayInformer
	GatewayInformerSynced cache.InformerSynced

	CurrentTargets []TerminationTarget
}

// Run - run it
func (d *Director) Run(stop <-chan struct{}) {
	zap.S().Info("Starting application synchronization")

	if !cache.WaitForCacheSync(stop, d.GatewayInformerSynced) {
		zap.S().Error("timed out waiting for cache sync")
		return
	}

	go d.syncWAFLoop(stop)
}

func (d *Director) syncRetryDelay() {
	time.Sleep(time.Second * 5)
}

func (d *Director) add(gw interface{}) {
	// zap.S().Infof("Add: %s", gw)
	d.update(nil, gw)
}

func (d *Director) update(old interface{}, new interface{}) {
	var gw, previous *istioApiv1alpha3.Gateway

	if old != nil {
		previous = old.(*istioApiv1alpha3.Gateway)
	}

	if previous != nil {
		// fmt.Printf("%s", previous)
	}

	gw = new.(*istioApiv1alpha3.Gateway)

	for _, srv := range gw.Spec.Servers {
		if srv.TLS != nil {
			zap.S().Info("Found TLS enabled port")
			secretName := srv.TLS.CredentialName

			target := TerminationTarget{
				Hosts:     srv.Hosts,
				Secret:    secretName,
				Target:    d.AzureWafConfig.BackendPool,
				Namespace: gw.Namespace,
			}

			zap.S().Debugf("Adding for %s for configuration with secret %s", target.Hosts, secretName)
			d.CurrentTargets = append(d.CurrentTargets, target)
		}
	}
}

func resourceRef(id string) *azureNetwork.SubResource {
	return &azureNetwork.SubResource{ID: to.StringPtr(id)}
}

func contains(data []string, element string) bool {
	for _, i := range data {
		if i == element {
			return true
		}
	}

	return false
}

func (d *Director) syncTargetsToWAF(waf *azureNetwork.ApplicationGateway) {
	wdPrefix := d.AzureWafConfig.ListenerPrefix
	listenersByName := map[string]azureNetwork.ApplicationGatewayHTTPListener{}

	/*
	 Loop through the settings we manage and keep the already existing
	 resources not prefixed by us.
	*/
	agCertificates := d.certificatesToSync(waf)
	agListeners := d.listenersToSync(waf, listenersByName)
	agRoutingRules := d.rulesToSync(waf)

	/*
		We are looking at the current Targets aka VirtualGateways and their secrets, from this
		we get the information needed to create AG Listeners and their Certifiates based on the k8s
		secrets.

		Tracking already added listeners & certificates as AG will fail if you add duplicates.
	*/
	addedListeners := make([]string, 0)
	addedCerts := make([]string, 0)
	for _, target := range d.CurrentTargets {
		rules := make([]azureNetwork.ApplicationGatewayRequestRoutingRule, 0)
		listeners := make([]azureNetwork.ApplicationGatewayHTTPListener, 0)

		/* Avoid duplicate secrets */
		secretName := target.generateSecretName(wdPrefix)
		if contains(addedCerts, secretName) {
			zap.S().Debugf("Skipping duplicate certificate %s", secretName)
			continue
		}
		addedCerts = append(addedCerts, secretName)

		for _, host := range target.Hosts {
			listener := d.targetListener(target, wdPrefix, waf, host)
			zap.S().Debugf("Syncing host:%s, listener:%s secret:%s", host, *listener.Name, target.Secret)

			if contains(addedListeners, *listener.Name) {
				zap.S().Debugf("Skipping duplicate listener %s", listener.Name)
				continue
			}
			addedListeners = append(addedListeners, *listener.Name)

			routingRule := d.targetRoutingRules(waf, listener, target)

			rules = append(rules, routingRule)
			listeners = append(listeners, listener)
		}

		secret, err := d.getSecretForTarget(target)
		if err != nil {
			zap.S().Infof("Error getting secret for listener %s, not added to listener list", target.Secret)
			zap.S().Error(err)
			continue
		}

		agCert, _ := d.convertCertificateToAGCertificate(target.generateSecretName(wdPrefix), secret)
		agCertificates = append(agCertificates, *agCert)
		agListeners = append(agListeners, listeners...)
		agRoutingRules = append(agRoutingRules, rules...)
	}

	waf.HTTPListeners = &agListeners
	waf.SslCertificates = &agCertificates
	waf.RequestRoutingRules = &agRoutingRules

	zap.S().Debugf("Have %d certificatesToSync", len(*waf.SslCertificates))
}

/*
	Convert a Secret to PFX
*/
func (d *Director) convertSecretCertToPfx(secret *v1.Secret) ([]byte, error) {
	wrapper, err := crypto.ParseSecretToCertContainer(secret)
	if err != nil {
		zap.S().Error(err)
		return nil, err
	}

	return sslMate.Encode(rand.Reader, wrapper.PrivateKey, wrapper.Certificates[0], wrapper.CACertificates, "azure")
}

/*
	Convert Certificate inside a Secret to AG Certificate object
*/
func (d *Director) convertCertificateToAGCertificate(secretName string, secret *v1.Secret) (*azureNetwork.ApplicationGatewaySslCertificate, error) {
	zap.S().Debugf("Converting certificate %s", secretName)
	certPfx, err := d.convertSecretCertToPfx(secret)
	if err != nil {
		return nil, err
	}

	certB64 := base64.StdEncoding.EncodeToString(certPfx)
	agCert := azureNetwork.ApplicationGatewaySslCertificate{
		Etag: to.StringPtr(""),
		Name: to.StringPtr(secretName),
		ApplicationGatewaySslCertificatePropertiesFormat: &azureNetwork.ApplicationGatewaySslCertificatePropertiesFormat{
			Data:     to.StringPtr(certB64),
			Password: to.StringPtr("azure"),
		},
	}

	return &agCert, nil
}

/*
	Fetch the given secret
*/
func (d *Director) getSecretForTarget(target TerminationTarget) (*v1.Secret, error) {
	secret, err := d.ClientSet.CoreV1().Secrets(target.Namespace).Get(target.Secret, metav1.GetOptions{})
	return secret, err
}

func (d *Director) targetRoutingRules(waf *azureNetwork.ApplicationGateway, listener azureNetwork.ApplicationGatewayHTTPListener, target TerminationTarget) azureNetwork.ApplicationGatewayRequestRoutingRule {
	httpListenerSubResource := azureNetwork.SubResource{ID: to.StringPtr(fmt.Sprintf("%s/httpListeners/%s", *waf.ID, *listener.Name))}
	routingRule := azureNetwork.ApplicationGatewayRequestRoutingRule{
		Etag: to.StringPtr("*"),
		Name: to.StringPtr(*listener.Name),
		ApplicationGatewayRequestRoutingRulePropertiesFormat: &azureNetwork.ApplicationGatewayRequestRoutingRulePropertiesFormat{
			RuleType:            azureNetwork.Basic,
			HTTPListener:        &httpListenerSubResource,
			BackendAddressPool:  resourceRef(fmt.Sprintf("%s/backendAddressPools/%s", *waf.ID, target.Target)),
			BackendHTTPSettings: resourceRef(fmt.Sprintf("%s/backendHttpSettingsCollection/%s", *waf.ID, d.AzureWafConfig.BackendHttpSettings)),
		},
	}

	return routingRule
}

func (d *Director) targetListener(target TerminationTarget, wdPrefix string, waf *azureNetwork.ApplicationGateway, host string) azureNetwork.ApplicationGatewayHTTPListener {
	listener := azureNetwork.ApplicationGatewayHTTPListener{}
	listenerName := fmt.Sprintf("%s-tls", target.generateNameWithPrefix(wdPrefix, host))
	listener.Name = &listenerName

	frontendIPRef := resourceRef(*(*waf.FrontendIPConfigurations)[0].ID)
	listener.ApplicationGatewayHTTPListenerPropertiesFormat = &azureNetwork.ApplicationGatewayHTTPListenerPropertiesFormat{
		FrontendIPConfiguration: frontendIPRef,
		FrontendPort:            resourceRef(fmt.Sprintf("%s/frontEndPorts/%s", *waf.ID, d.AzureWafConfig.FrontendPort)),
		HostName:                to.StringPtr(host),
		Protocol:                azureNetwork.HTTPS,
		SslCertificate:          resourceRef(fmt.Sprintf("%s/sslCertificates/%s", *waf.ID, target.generateSecretName(wdPrefix))),
	}

	return listener
}

func (d *Director) rulesToSync(waf *azureNetwork.ApplicationGateway) []azureNetwork.ApplicationGatewayRequestRoutingRule {
	routingRules := []azureNetwork.ApplicationGatewayRequestRoutingRule{}

	for _, rr := range *waf.RequestRoutingRules {
		if !d.hasPrefix(*rr.Name) {
			routingRules = append(routingRules, rr)
		} else {
			zap.S().Debugf("Skipping rule %s", *rr.Name)
		}
	}

	return routingRules
}

func (d *Director) listenersToSync(waf *azureNetwork.ApplicationGateway, listenersByName map[string]azureNetwork.ApplicationGatewayHTTPListener) []azureNetwork.ApplicationGatewayHTTPListener {
	listeners := []azureNetwork.ApplicationGatewayHTTPListener{}
	for _, l := range *waf.HTTPListeners {
		if !d.hasPrefix(*l.Name) {
			listeners = append(listeners, l)
			listenersByName[*l.Name] = l
		} else {
			zap.S().Debugf("Skipping listener %s", *l.Name)
		}
	}

	return listeners
}

func (d *Director) certificatesToSync(waf *azureNetwork.ApplicationGateway) []azureNetwork.ApplicationGatewaySslCertificate {
	sslCertificates := []azureNetwork.ApplicationGatewaySslCertificate{}
	for _, sslCert := range *waf.SslCertificates {
		if !d.hasPrefix(*sslCert.Name) {
			sslCertificates = append(sslCertificates, sslCert)
		} else {
			zap.S().Debugf("Skipping certificate %s", *sslCert.Name)
		}
	}

	return sslCertificates
}

func (d *Director) hasPrefix(name string) bool {
	wdPrefix := d.AzureWafConfig.ListenerPrefix
	return strings.HasPrefix(name, wdPrefix)
}

func (d *Director) syncWAFLoop(stop <-chan struct{}) {
	agName := d.AzureWafConfig.Name
	agRgName := d.AzureWafConfig.ResourceGroup

	var (
		updateFuture azureNetwork.ApplicationGatewaysCreateOrUpdateFuture
	)

	for {
		waf, err := d.AzureAGClient.Get(context.Background(), agRgName, agName)

		if err != nil {
			zap.S().Error(err)
			zap.S().Infof("Error getting WAF %s %s", agRgName, agName)
			d.syncRetryDelay()
			continue
		}

		if *waf.ProvisioningState == "Updating" {
			zap.S().Debugf("WAF is updating, sleeping.")
			d.syncRetryDelay()
			continue
		}

		d.syncTargetsToWAF(&waf)
		zap.S().Info("Updating WAF")

		updateFuture, err = d.AzureAGClient.CreateOrUpdate(context.Background(), agRgName, agName, waf)
		if err != nil {
			zap.S().Error(err)
		}

		err = updateFuture.WaitForCompletionRef(context.Background(), d.AzureAGClient.Client)
		if err != nil {
			zap.S().Error(err)
		}

		zap.S().Info("Successfully updated WAF")

		time.Sleep(30 * time.Second)
	}
}

// NewDirector - Creates a new instance of the director
func NewDirector(
	k8sClient *kubernetes.Clientset, istioClient *istio.Clientset, agClient *azureNetwork.ApplicationGatewaysClient, gwInformer v1alpha3.GatewayInformer) *Director {
	azureConfig := config.NewAzureConfig()
	director := &Director{
		AzureWafConfig:        azureConfig,
		AzureAGClient:         agClient,
		ClientSet:             k8sClient,
		IstioClient:           istioClient,
		GatewayInformer:       gwInformer,
		GatewayInformerSynced: gwInformer.Informer().HasSynced,
		CurrentTargets:        make([]TerminationTarget, 0),
	}

	gwInformer.Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(newPod interface{}) {
				director.add(newPod)
			},
			UpdateFunc: func(oldGw, newGw interface{}) {
				director.update(oldGw, newGw)
			},
		})

	return director
}
