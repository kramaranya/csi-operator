package openstack_cinder

import (
	"context"
	"fmt"
	"time"

	opv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/csi-operator/assets"
	"github.com/openshift/csi-operator/pkg/clients"
	commongenerator "github.com/openshift/csi-operator/pkg/driver/common/generator"
	"github.com/openshift/csi-operator/pkg/driver/common/operator"
	"github.com/openshift/csi-operator/pkg/generator"
	configsync "github.com/openshift/csi-operator/pkg/openstack-cinder/config"
	"github.com/openshift/csi-operator/pkg/operator/config"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivercontrollerservicecontroller"
	"github.com/openshift/library-go/pkg/operator/csi/csidrivernodeservicecontroller"
	dc "github.com/openshift/library-go/pkg/operator/deploymentcontroller"
	"k8s.io/klog/v2"
)

const (
	resyncInterval        = 20 * time.Minute
	cinderConfigName      = "cloud-conf"
	cloudCredSecretName   = "openstack-cloud-credentials"
	metricsCertSecretName = "openstack-cinder-csi-driver-controller-metrics-serving-cert"
	infrastructureName    = "cluster"
	cloudConfigNamespace  = "openshift-config-managed"
	cloudConfigName       = "kube-cloud-config"
	caBundleKey           = "ca-bundle.pem"
	trustedCAConfigMap    = "openstack-cinder-csi-driver-trusted-ca-bundle"

	openshiftDefaultCloudConfigNamespace = "openshift-config"

	generatedAssetBase = "overlays/openstack-cinder/generated"
)

// GetOpenStackCinderGeneratorConfig returns configuration for generating assets of Cinder CSI driver operator.
func GetOpenStackCinderGeneratorConfig() *generator.CSIDriverGeneratorConfig {
	return &generator.CSIDriverGeneratorConfig{
		AssetPrefix:      "openstack-cinder-csi-driver",
		AssetShortPrefix: "openstack-cinder",
		DriverName:       "cinder.csi.openstack.org",
		OutputDir:        generatedAssetBase,

		ControllerConfig: &generator.ControlPlaneConfig{
			DeploymentTemplateAssetName: "overlays/openstack-cinder/patches/controller_add_driver.yaml",
			LivenessProbePort:           10301,
			MetricsPorts: []generator.MetricsPort{
				{
					LocalPort:           commongenerator.OpenStackCinderLoopbackMetricsPortStart,
					InjectKubeRBACProxy: true,
					ExposedPort:         commongenerator.OpenStackCinderExposedMetricsPortStart,
					Name:                "driver-m",
				},
			},
			SidecarLocalMetricsPortStart:   commongenerator.OpenStackCinderLoopbackMetricsPortStart + 1,
			SidecarExposedMetricsPortStart: commongenerator.OpenStackCinderExposedMetricsPortStart + 1,
			Sidecars: []generator.SidecarConfig{
				commongenerator.DefaultProvisionerWithSnapshots.WithExtraArguments(
					"--timeout=3m",
					"--feature-gates=Topology=$(ENABLE_TOPOLOGY)",
					"--default-fstype=ext4",
				).WithPatches(generator.StandaloneOnly,
					"controller.yaml", "overlays/openstack-cinder/patches/provisioner_add_envvars.yaml",
				),
				commongenerator.DefaultAttacher.WithExtraArguments(
					"--timeout=3m",
				),
				// FIXME(stephenfin): Unlike other sidecars, this one doesn't (and didn't) set a timeout. Should it?
				commongenerator.DefaultResizer.WithExtraArguments(),
				// FIXME(stephenfin): Unlike other sidecars, this one doesn't (and didn't) set a timeout. Should it?
				commongenerator.DefaultSnapshotter.WithExtraArguments(),
				commongenerator.DefaultLivenessProbe.WithExtraArguments(
					"--probe-timeout=10s",
				),
			},
			Assets:       commongenerator.DefaultControllerAssets,
			AssetPatches: commongenerator.DefaultAssetPatches,
		},

		GuestConfig: &generator.GuestConfig{
			DaemonSetTemplateAssetName:   "overlays/openstack-cinder/patches/node_add_driver.yaml",
			LivenessProbePort:            10300,
			NodeRegistrarHealthCheckPort: 10304,
			Sidecars: []generator.SidecarConfig{
				commongenerator.DefaultLivenessProbe.WithExtraArguments(
					"--probe-timeout=10s",
				),
				commongenerator.DefaultNodeDriverRegistrar,
			},
			Assets: commongenerator.DefaultNodeAssets.WithAssets(generator.AllFlavours,
				"overlays/openstack-cinder/base/csidriver.yaml",
				"overlays/openstack-cinder/base/storageclass.yaml",
				"overlays/openstack-cinder/base/volumesnapshotclass.yaml",
			),
		},

		// TODO(stephenfin): We'd like to support HyperShift. When we do, we should change this.
		StandaloneOnly: true,
	}
}

// GetOpenStackCinderOperatorConfig returns runtime configuration of the CSI driver operator.
func GetOpenStackCinderOperatorConfig() *config.OperatorConfig {
	return &config.OperatorConfig{
		CSIDriverName:                   opv1.CinderCSIDriver,
		UserAgent:                       "openstack-cinder-csi-driver-operator",
		AssetReader:                     assets.ReadFile,
		AssetDir:                        generatedAssetBase,
		CloudConfigNamespace:            openshiftDefaultCloudConfigNamespace,
		OperatorControllerConfigBuilder: GetOpenStackCinderOperatorControllerConfig,
		Removable:                       false,
	}
}

// GetOpenStackCinderOperatorControllerConfig returns second half of runtime configuration of the CSI driver operator,
// after a client connection + cluster flavour are established.
func GetOpenStackCinderOperatorControllerConfig(ctx context.Context, flavour generator.ClusterFlavour, c *clients.Clients) (*config.OperatorControllerConfig, error) {
	if flavour != generator.FlavourStandalone {
		klog.Error(nil, "Flavour HyperShift is not supported!")
		return nil, fmt.Errorf("Flavour HyperShift is not supported!")
	}

	cfg := operator.NewDefaultOperatorControllerConfig(flavour, c, "OpenStackCinder")

	go c.ConfigInformers.Start(ctx.Done())

	// Hooks to run on all clusters
	cfg.AddDeploymentHookBuilders(c, withCABundleDeploymentHook, withConfigDeploymentHook)
	cfg.AddDaemonSetHookBuilders(c, withCABundleDaemonSetHook, withConfigDaemonSetHook)

	cfg.DeploymentWatchedSecretNames = append(cfg.DeploymentWatchedSecretNames, cloudCredSecretName, metricsCertSecretName)

	configMapSyncer, err := createConfigMapSyncer(c)
	if err != nil {
		return nil, err
	}
	cfg.ExtraControlPlaneControllers = append(cfg.ExtraControlPlaneControllers, configMapSyncer)

	return cfg, nil
}

// withCABundleDeploymentHook projects custom CA bundle ConfigMap into the CSI driver container
func withCABundleDeploymentHook(c *clients.Clients) (dc.DeploymentHookFunc, []factory.Informer) {
	hook := csidrivercontrollerservicecontroller.WithCABundleDeploymentHook(
		c.ControlPlaneNamespace,
		trustedCAConfigMap,
		c.GetControlPlaneConfigMapInformer(c.ControlPlaneNamespace),
	)
	informers := []factory.Informer{
		c.GetControlPlaneConfigMapInformer(c.ControlPlaneNamespace).Informer(),
	}
	return hook, informers
}

// withCABundleDaemonSetHook projects custom CA bundle ConfigMap into the CSI driver container
func withCABundleDaemonSetHook(c *clients.Clients) (csidrivernodeservicecontroller.DaemonSetHookFunc, []factory.Informer) {
	hook := csidrivernodeservicecontroller.WithCABundleDaemonSetHook(
		clients.CSIDriverNamespace,
		trustedCAConfigMap,
		c.GetConfigMapInformer(clients.CSIDriverNamespace),
	)
	informers := []factory.Informer{
		c.GetConfigMapInformer(clients.CSIDriverNamespace).Informer(),
	}
	return hook, informers
}

// withConfigDeploymentHook adds annotations based on the hash of the config map containing our config,
// ensuring we restart if that changes
func withConfigDeploymentHook(c *clients.Clients) (dc.DeploymentHookFunc, []factory.Informer) {
	hook := csidrivercontrollerservicecontroller.WithConfigMapHashAnnotationHook(
		c.ControlPlaneNamespace,
		cinderConfigName,
		c.GetControlPlaneConfigMapInformer(c.ControlPlaneNamespace),
	)
	informers := []factory.Informer{}
	return hook, informers
}

// withConfigDeploymentHook adds annotations based on the hash of the config map containing our config,
// ensuring we restart if that changes
func withConfigDaemonSetHook(c *clients.Clients) (csidrivernodeservicecontroller.DaemonSetHookFunc, []factory.Informer) {
	hook := csidrivernodeservicecontroller.WithConfigMapHashAnnotationHook(
		c.ControlPlaneNamespace,
		cinderConfigName,
		c.GetControlPlaneConfigMapInformer(c.ControlPlaneNamespace),
	)
	informers := []factory.Informer{}
	return hook, informers
}

// createConfigMapSyncer syncs config maps containing configuration for the CSI driver from the
// user-managed namespace to the operator namespace, validating it and potentially transforming it
// in the process
func createConfigMapSyncer(c *clients.Clients) (factory.Controller, error) {
	configSyncController := configsync.NewConfigSyncController(
		c.OperatorClient,
		c.KubeClient,
		c.KubeInformers,
		c.ConfigInformers,
		resyncInterval,
		c.EventRecorder)

	return configSyncController, nil
}
