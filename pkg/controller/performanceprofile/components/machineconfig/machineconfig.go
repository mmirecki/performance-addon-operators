package machineconfig

import (
	"encoding/base64"
	"fmt"
	"io/ioutil"

	"github.com/coreos/go-systemd/unit"
	igntypes "github.com/coreos/ignition/config/v2_2/types"

	performancev1alpha1 "github.com/openshift-kni/performance-addon-operators/pkg/apis/performance/v1alpha1"
	"github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components"
	profile2 "github.com/openshift-kni/performance-addon-operators/pkg/controller/performanceprofile/components/profile"

	machineconfigv1 "github.com/openshift/machine-config-operator/pkg/apis/machineconfiguration.openshift.io/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
)

const (
	defaultIgnitionVersion       = "2.2.0"
	defaultFileSystem            = "root"
	defaultIgnitionContentSource = "data:text/plain;charset=utf-8;base64"
)

const (
	// Values of the kernel setting in MachineConfig, unfortunately not exported by MCO
	mcKernelRT      = "realtime"
	mcKernelDefault = "default"

	preBootTuning  = "pre-boot-tuning"
	reboot         = "reboot"
	bashScriptsDir = "/usr/local/bin"
)

const (
	systemdSectionUnit     = "Unit"
	systemdSectionService  = "Service"
	systemdSectionInstall  = "Install"
	systemdDescription     = "Description"
	systemdWants           = "Wants"
	systemdAfter           = "After"
	systemdBefore          = "Before"
	systemdEnvironment     = "Environment"
	systemdType            = "Type"
	systemdRemainAfterExit = "RemainAfterExit"
	systemdExecStart       = "ExecStart"
	systemdWantedBy        = "WantedBy"
)

const (
	systemdServiceKubelet      = "kubelet.service"
	systemdServiceTypeOneshot  = "oneshot"
	systemdTargetMultiUser     = "multi-user.target"
	systemdTargetNetworkOnline = "network-online.target"
	systemdTrue                = "true"
)

const (
	environmentNonIsolatedCpus = "NON_ISOLATED_CPUS"
)

// New returns new machine configuration object for performance sensetive workflows
func New(assetsDir string, profile *performancev1alpha1.PerformanceProfile) (*machineconfigv1.MachineConfig, error) {
	name := components.GetComponentName(profile.Name, components.ComponentNamePrefix)
	mc := &machineconfigv1.MachineConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: machineconfigv1.GroupVersion.String(),
			Kind:       "MachineConfig",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: profile2.GetMachineConfigLabel(*profile),
		},
		Spec: machineconfigv1.MachineConfigSpec{},
	}

	ignitionConfig, err := getIgnitionConfig(assetsDir, profile)
	if err != nil {
		return nil, err
	}

	mc.Spec.Config = *ignitionConfig
	mc.Spec.KernelArguments = getKernelArgs(profile.Spec.HugePages, profile.Spec.CPU.Isolated)

	enableRTKernel := profile.Spec.RealTimeKernel != nil &&
		profile.Spec.RealTimeKernel.Enabled != nil &&
		*profile.Spec.RealTimeKernel.Enabled

	if enableRTKernel {
		mc.Spec.KernelType = mcKernelRT
	} else {
		mc.Spec.KernelType = mcKernelDefault
	}

	return mc, nil
}

func getKernelArgs(hugePages *performancev1alpha1.HugePages, isolatedCPUs *performancev1alpha1.CPUSet) []string {
	kargs := []string{
		"nohz=on",
		"nosoftlockup",
		"nmi_watchdog=0",
		"audit=0",
		"mce=off",
		"irqaffinity=0",
		"skew_tick=1",
		"processor.max_cstate=1",
		"idle=poll",
		"intel_pstate=disable",
		"intel_idle.max_cstate=0",
		"intel_iommu=on",
		"iommu=pt",
	}

	if isolatedCPUs != nil {
		kargs = append(kargs, fmt.Sprintf("isolcpus=%s", string(*isolatedCPUs)))
	}

	if hugePages != nil {
		if hugePages.DefaultHugePagesSize != nil {
			kargs = append(kargs, fmt.Sprintf("default_hugepagesz=%s", string(*hugePages.DefaultHugePagesSize)))
		}

		for _, page := range hugePages.Pages {
			kargs = append(kargs, fmt.Sprintf("hugepagesz=%s", string(page.Size)))
			kargs = append(kargs, fmt.Sprintf("hugepages=%d", page.Count))
		}
	}
	return kargs
}

func getIgnitionConfig(assetsDir string, profile *performancev1alpha1.PerformanceProfile) (*igntypes.Config, error) {

	mode := 0700
	ignitionConfig := &igntypes.Config{
		Ignition: igntypes.Ignition{
			Version: defaultIgnitionVersion,
		},
		Storage: igntypes.Storage{
			Files: []igntypes.File{},
		},
	}

	for _, script := range []string{preBootTuning, reboot} {
		content, err := ioutil.ReadFile(fmt.Sprintf("%s/scripts/%s.sh", assetsDir, script))
		if err != nil {
			return nil, err
		}
		contentBase64 := base64.StdEncoding.EncodeToString(content)
		ignitionConfig.Storage.Files = append(ignitionConfig.Storage.Files, igntypes.File{
			Node: igntypes.Node{
				Filesystem: defaultFileSystem,
				Path:       getBashScriptPath(script),
			},
			FileEmbedded1: igntypes.FileEmbedded1{
				Contents: igntypes.FileContents{
					Source: fmt.Sprintf("%s,%s", defaultIgnitionContentSource, contentBase64),
				},
				Mode: &mode,
			},
		})
	}

	nonIsolatedCpus := profile.Spec.CPU.NonIsolated
	preBootTuningService, err := getSystemdContent(
		getPreBootTuningUnitOptions(string(*nonIsolatedCpus)),
	)
	if err != nil {
		return nil, err
	}

	rebootService, err := getSystemdContent(getRebootUnitOptions())
	if err != nil {
		return nil, err
	}

	ignitionConfig.Systemd = igntypes.Systemd{
		Units: []igntypes.Unit{
			{
				Contents: preBootTuningService,
				Enabled:  pointer.BoolPtr(true),
				Name:     getSystemdService(preBootTuning),
			},
			{
				Contents: rebootService,
				Enabled:  pointer.BoolPtr(true),
				Name:     getSystemdService(reboot),
			},
		},
	}
	return ignitionConfig, nil
}

func getBashScriptPath(scriptName string) string {
	return fmt.Sprintf("%s/%s.sh", bashScriptsDir, scriptName)
}

func getSystemdEnvironment(key string, value string) string {
	return fmt.Sprintf("%s=%s", key, value)
}

func getSystemdService(serviceName string) string {
	return fmt.Sprintf("%s.service", serviceName)
}

func getSystemdContent(options []*unit.UnitOption) (string, error) {
	outReader := unit.Serialize(options)
	outBytes, err := ioutil.ReadAll(outReader)
	if err != nil {
		return "", err
	}
	return string(outBytes), nil
}

func getRebootUnitOptions() []*unit.UnitOption {
	return []*unit.UnitOption{
		// [Unit]
		// Description
		unit.NewUnitOption(systemdSectionUnit, systemdDescription, "Reboot initiated by pre-boot-tuning"),
		// Wants
		unit.NewUnitOption(systemdSectionUnit, systemdWants, systemdTargetNetworkOnline),
		// After
		unit.NewUnitOption(systemdSectionUnit, systemdAfter, systemdTargetNetworkOnline),
		// Before
		unit.NewUnitOption(systemdSectionUnit, systemdBefore, systemdServiceKubelet),
		// [Service]
		// Type
		unit.NewUnitOption(systemdSectionService, systemdType, systemdServiceTypeOneshot),
		// RemainAfterExit
		unit.NewUnitOption(systemdSectionService, systemdRemainAfterExit, systemdTrue),
		// ExecStart
		unit.NewUnitOption(systemdSectionService, systemdExecStart, getBashScriptPath(reboot)),
		// [Install]
		// WantedBy
		unit.NewUnitOption(systemdSectionInstall, systemdWantedBy, systemdTargetMultiUser),
	}
}

func getPreBootTuningUnitOptions(nonIsolatedCpus string) []*unit.UnitOption {
	return []*unit.UnitOption{
		// [Unit]
		// Description
		unit.NewUnitOption(systemdSectionUnit, systemdDescription, "Preboot tuning patch"),
		// Before
		unit.NewUnitOption(systemdSectionUnit, systemdBefore, systemdServiceKubelet),
		unit.NewUnitOption(systemdSectionUnit, systemdBefore, getSystemdService(reboot)),
		// [Service]
		// Environment
		unit.NewUnitOption(systemdSectionService, systemdEnvironment, getSystemdEnvironment(environmentNonIsolatedCpus, nonIsolatedCpus)),
		// Type
		unit.NewUnitOption(systemdSectionService, systemdType, systemdServiceTypeOneshot),
		// RemainAfterExit
		unit.NewUnitOption(systemdSectionService, systemdRemainAfterExit, systemdTrue),
		// ExecStart
		unit.NewUnitOption(systemdSectionService, systemdExecStart, getBashScriptPath(preBootTuning)),
		// [Install]
		// WantedBy
		unit.NewUnitOption(systemdSectionInstall, systemdWantedBy, systemdTargetMultiUser),
	}
}