/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2018 Red Hat, Inc.
 *
 */

package virt_operator

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	golog "log"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/pflag"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "kubevirt.io/client-go/api/v1"
	"kubevirt.io/kubevirt/pkg/certificates/triple"
	"kubevirt.io/kubevirt/pkg/certificates/triple/cert"
	"kubevirt.io/kubevirt/pkg/util/webhooks"
	validating_webhooks "kubevirt.io/kubevirt/pkg/util/webhooks/validating-webhooks"
	operator_webhooks "kubevirt.io/kubevirt/pkg/virt-operator/webhooks"

	k8sv1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	k8coresv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	clientrest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"

	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/log"
	clientutil "kubevirt.io/client-go/util"
	"kubevirt.io/kubevirt/pkg/certificates"
	"kubevirt.io/kubevirt/pkg/controller"
	"kubevirt.io/kubevirt/pkg/service"
	clusterutil "kubevirt.io/kubevirt/pkg/util/cluster"
	"kubevirt.io/kubevirt/pkg/virt-controller/leaderelectionconfig"
	installstrategy "kubevirt.io/kubevirt/pkg/virt-operator/install-strategy"
	"kubevirt.io/kubevirt/pkg/virt-operator/util"
)

const (
	controllerThreads = 3

	// Default port that virt-operator listens on.
	defaultPort = 8186

	// Default address that virt-operator listens on.
	defaultHost = "0.0.0.0"

	// selfsigned cert secret name
	virtOperatorCertSecretName = "kubevirt-operator-certs"

	certBytesValue        = "cert-bytes"
	keyBytesValue         = "key-bytes"
	signingCertBytesValue = "signing-cert-bytes"
)

type VirtOperatorApp struct {
	service.ServiceListen

	clientSet       kubecli.KubevirtClient
	restClient      *clientrest.RESTClient
	informerFactory controller.KubeInformerFactory

	kubeVirtController KubeVirtController
	kubeVirtRecorder   record.EventRecorder

	operatorNamespace string

	kubeVirtInformer cache.SharedIndexInformer
	kubeVirtCache    cache.Store

	stores    util.Stores
	informers util.Informers

	LeaderElection   leaderelectionconfig.Configuration
	certBytes        []byte
	keyBytes         []byte
	signingCertBytes []byte
}

var _ service.Service = &VirtOperatorApp{}

func Execute() {
	var err error
	app := VirtOperatorApp{}

	dumpInstallStrategy := pflag.Bool("dump-install-strategy", false, "Dump install strategy to configmap and exit")

	service.Setup(&app)

	log.InitializeLogging("virt-operator")

	err = util.VerifyEnv()
	if err != nil {
		golog.Fatal(err)
	}

	app.clientSet, err = kubecli.GetKubevirtClient()

	if err != nil {
		golog.Fatal(err)
	}

	app.restClient = app.clientSet.RestClient()

	app.LeaderElection = leaderelectionconfig.DefaultLeaderElectionConfiguration()

	app.operatorNamespace, err = clientutil.GetNamespace()
	if err != nil {
		golog.Fatalf("Error searching for namespace: %v", err)
	}

	if *dumpInstallStrategy {
		err = installstrategy.DumpInstallStrategyToConfigMap(app.clientSet, app.operatorNamespace)
		if err != nil {
			golog.Fatal(err)
		}
		os.Exit(0)
	}

	app.informerFactory = controller.NewKubeInformerFactory(app.restClient, app.clientSet, app.operatorNamespace)

	app.kubeVirtInformer = app.informerFactory.KubeVirt()
	app.kubeVirtCache = app.kubeVirtInformer.GetStore()

	app.informers = util.Informers{
		ServiceAccount:           app.informerFactory.OperatorServiceAccount(),
		ClusterRole:              app.informerFactory.OperatorClusterRole(),
		ClusterRoleBinding:       app.informerFactory.OperatorClusterRoleBinding(),
		Role:                     app.informerFactory.OperatorRole(),
		RoleBinding:              app.informerFactory.OperatorRoleBinding(),
		Crd:                      app.informerFactory.OperatorCRD(),
		Service:                  app.informerFactory.OperatorService(),
		Deployment:               app.informerFactory.OperatorDeployment(),
		DaemonSet:                app.informerFactory.OperatorDaemonSet(),
		ValidationWebhook:        app.informerFactory.OperatorValidationWebhook(),
		InstallStrategyConfigMap: app.informerFactory.OperatorInstallStrategyConfigMaps(),
		InstallStrategyJob:       app.informerFactory.OperatorInstallStrategyJob(),
		InfrastructurePod:        app.informerFactory.OperatorPod(),
		PodDisruptionBudget:      app.informerFactory.OperatorPodDisruptionBudget(),
		Namespace:                app.informerFactory.Namespace(),
	}

	app.stores = util.Stores{
		ServiceAccountCache:           app.informerFactory.OperatorServiceAccount().GetStore(),
		ClusterRoleCache:              app.informerFactory.OperatorClusterRole().GetStore(),
		ClusterRoleBindingCache:       app.informerFactory.OperatorClusterRoleBinding().GetStore(),
		RoleCache:                     app.informerFactory.OperatorRole().GetStore(),
		RoleBindingCache:              app.informerFactory.OperatorRoleBinding().GetStore(),
		CrdCache:                      app.informerFactory.OperatorCRD().GetStore(),
		ServiceCache:                  app.informerFactory.OperatorService().GetStore(),
		DeploymentCache:               app.informerFactory.OperatorDeployment().GetStore(),
		DaemonSetCache:                app.informerFactory.OperatorDaemonSet().GetStore(),
		ValidationWebhookCache:        app.informerFactory.OperatorValidationWebhook().GetStore(),
		InstallStrategyConfigMapCache: app.informerFactory.OperatorInstallStrategyConfigMaps().GetStore(),
		InstallStrategyJobCache:       app.informerFactory.OperatorInstallStrategyJob().GetStore(),
		InfrastructurePodCache:        app.informerFactory.OperatorPod().GetStore(),
		PodDisruptionBudgetCache:      app.informerFactory.OperatorPodDisruptionBudget().GetStore(),
		NamespaceCache:                app.informerFactory.Namespace().GetStore(),
	}

	onOpenShift, err := clusterutil.IsOnOpenShift(app.clientSet)
	if err != nil {
		golog.Fatalf("Error determining cluster type: %v", err)
	}
	if onOpenShift {
		log.Log.Info("we are on openshift")
		app.informers.SCC = app.informerFactory.OperatorSCC()
		app.stores.SCCCache = app.informerFactory.OperatorSCC().GetStore()
		app.stores.IsOnOpenshift = true
	} else {
		log.Log.Info("we are on kubernetes")
		app.informers.SCC = app.informerFactory.DummyOperatorSCC()
		app.stores.SCCCache = app.informerFactory.DummyOperatorSCC().GetStore()
	}

	serviceMonitorEnabled, err := util.IsServiceMonitorEnabled(app.clientSet)
	if err != nil {
		golog.Fatalf("Error checking for ServiceMonitor: %v", err)
	}
	if serviceMonitorEnabled {
		log.Log.Info("servicemonitor is defined")
		app.informers.ServiceMonitor = app.informerFactory.OperatorServiceMonitor()
		app.stores.ServiceMonitorCache = app.informerFactory.OperatorServiceMonitor().GetStore()

		app.stores.ServiceMonitorEnabled = true
	} else {
		log.Log.Info("servicemonitor is not defined")
		app.informers.ServiceMonitor = app.informerFactory.DummyOperatorServiceMonitor()
		app.stores.ServiceMonitorCache = app.informerFactory.DummyOperatorServiceMonitor().GetStore()
	}

	if err = app.getSelfSignedCert(); err != nil {
		panic(err)
	}

	app.kubeVirtRecorder = app.getNewRecorder(k8sv1.NamespaceAll, "virt-operator")
	app.kubeVirtController = *NewKubeVirtController(app.clientSet, app.kubeVirtInformer, app.kubeVirtRecorder, app.stores, app.informers, app.operatorNamespace, app.signingCertBytes)

	image := os.Getenv(util.OperatorImageEnvName)
	if image == "" {
		golog.Fatalf("Error getting operator's image: %v", err)
	}
	log.Log.Infof("Operator image: %s", image)

	app.Run()
}

func (app *VirtOperatorApp) Run() {

	// prepare certs
	certsDirectory, err := ioutil.TempDir("", "certsdir")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(certsDirectory)

	certStore, err := certificates.GenerateSelfSignedCert(certsDirectory, "virt-operator", app.operatorNamespace)
	if err != nil {
		log.Log.Reason(err).Error("unable to generate certificates")
		panic(err)
	}

	go func() {
		// serve metrics
		http.Handle("/metrics", promhttp.Handler())
		err = http.ListenAndServeTLS(app.ServiceListen.Address(), certStore.CurrentPath(), certStore.CurrentPath(), nil)
		if err != nil {
			log.Log.Reason(err).Error("Serving prometheus failed.")
			panic(err)
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	endpointName := "virt-operator"

	recorder := app.getNewRecorder(k8sv1.NamespaceAll, endpointName)

	id, err := os.Hostname()
	if err != nil {
		golog.Fatalf("unable to get hostname: %v", err)
	}

	rl, err := resourcelock.New(app.LeaderElection.ResourceLock,
		app.operatorNamespace,
		endpointName,
		app.clientSet.CoreV1(),
		app.clientSet.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: recorder,
		})
	if err != nil {
		golog.Fatal(err)
	}

	apiAuthConfig := app.informerFactory.ApiAuthConfigMap()

	stop := ctx.Done()
	app.informerFactory.Start(stop)
	cache.WaitForCacheSync(stop, apiAuthConfig.HasSynced)

	caManager := webhooks.NewClientCAManager(apiAuthConfig.GetStore())
	certPair, err := tls.X509KeyPair(app.certBytes, app.keyBytes)
	if err != nil {
		panic(err)
	}

	tlsConfig := webhooks.SetupTLS(caManager, certPair, tls.VerifyClientCertIfGiven)

	webhookServer := &http.Server{
		Addr:      fmt.Sprintf("%s:%d", app.BindAddress, 8444),
		TLSConfig: tlsConfig,
	}

	var mux http.ServeMux
	mux.HandleFunc("/kubevirt-validate-delete", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		validating_webhooks.Serve(w, r, operator_webhooks.NewKubeVirtDeletionAdmitter(app.clientSet))
	}))
	webhookServer.Handler = &mux
	go func() {
		err := webhookServer.ListenAndServeTLS("", "")
		if err != nil {
			panic(err)
		}
	}()

	leaderElector, err := leaderelection.NewLeaderElector(
		leaderelection.LeaderElectionConfig{
			Lock:          rl,
			LeaseDuration: app.LeaderElection.LeaseDuration.Duration,
			RenewDeadline: app.LeaderElection.RenewDeadline.Duration,
			RetryPeriod:   app.LeaderElection.RetryPeriod.Duration,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					log.Log.Infof("Started leading")
					// run app
					go app.kubeVirtController.Run(controllerThreads, stop)
				},
				OnStoppedLeading: func() {
					golog.Fatal("leaderelection lost")
				},
			},
		})
	if err != nil {
		golog.Fatal(err)
	}

	log.Log.Infof("Attempting to aquire leader status")
	leaderElector.Run(ctx)
	panic("unreachable")

}

func (app *VirtOperatorApp) getNewRecorder(namespace string, componentName string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&k8coresv1.EventSinkImpl{Interface: app.clientSet.CoreV1().Events(namespace)})
	return eventBroadcaster.NewRecorder(scheme.Scheme, k8sv1.EventSource{Component: componentName})
}

func (app *VirtOperatorApp) AddFlags() {
	app.InitFlags()

	app.BindAddress = defaultHost
	app.Port = defaultPort

	app.AddCommonFlags()
}

func (app *VirtOperatorApp) getSelfSignedCert() error {
	var ok bool

	caKeyPair, _ := triple.NewCA("kubevirt.io")
	keyPair, _ := triple.NewServerKeyPair(
		caKeyPair,
		"kubevirt-operator-webhook."+app.operatorNamespace+".pod.cluster.local",
		"kubevirt-operator-webhook",
		app.operatorNamespace,
		"cluster.local",
		nil,
		nil,
	)

	secret := &k8sv1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      virtOperatorCertSecretName,
			Namespace: app.operatorNamespace,
			Labels: map[string]string{
				v1.AppLabel: "virt-operator-webhooks",
			},
		},
		Type: "Opaque",
		Data: map[string][]byte{
			certBytesValue:        cert.EncodeCertPEM(keyPair.Cert),
			keyBytesValue:         cert.EncodePrivateKeyPEM(keyPair.Key),
			signingCertBytesValue: cert.EncodeCertPEM(caKeyPair.Cert),
		},
	}
	_, err := app.clientSet.CoreV1().Secrets(app.operatorNamespace).Create(secret)
	if errors.IsAlreadyExists(err) {
		secret, err = app.clientSet.CoreV1().Secrets(app.operatorNamespace).Get(virtOperatorCertSecretName, metav1.GetOptions{})
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// retrieve self signed cert info from secret

	app.certBytes, ok = secret.Data[certBytesValue]
	if !ok {
		return fmt.Errorf("%s value not found in %s virt-api secret", certBytesValue, virtOperatorCertSecretName)
	}
	app.keyBytes, ok = secret.Data[keyBytesValue]
	if !ok {
		return fmt.Errorf("%s value not found in %s virt-api secret", keyBytesValue, virtOperatorCertSecretName)
	}
	app.signingCertBytes, ok = secret.Data[signingCertBytesValue]
	if !ok {
		return fmt.Errorf("%s value not found in %s virt-api secret", signingCertBytesValue, virtOperatorCertSecretName)
	}
	return nil
}
