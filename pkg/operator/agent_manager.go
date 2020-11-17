package operator

import (
	"context"
	"fmt"

	"github.com/blang/semver"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/klog/v2"

	"github.com/kinvolk/flatcar-linux-update-operator/pkg/constants"
	"github.com/kinvolk/flatcar-linux-update-operator/pkg/k8sutil"
	"github.com/kinvolk/flatcar-linux-update-operator/pkg/version"
)

var (
	daemonsetName = "flatcar-linux-update-agent-ds"

	managedByOperatorLabels = map[string]string{
		"managed-by": "flatcar-linux-update-operator",
		"app":        agentDefaultAppName,
	}

	// Labels nodes where update-agent should be scheduled.
	enableUpdateAgentLabel = map[string]string{
		constants.LabelUpdateAgentEnabled: constants.True,
	}

	// Label Requirement matching nodes which lack the update agent label.
	updateAgentLabelMissing = k8sutil.NewRequirementOrDie(
		constants.LabelUpdateAgentEnabled,
		selection.DoesNotExist,
		[]string{},
	)
)

// legacyLabeler finds Flatcar Container Linux nodes lacking the update-agent enabled
// label and adds the label set "true" so nodes opt-in to running update-agent.
//
// Important: This behavior supports clusters which may have nodes that do not
// have labels which an update-agent daemonset might node select upon. Even if
// all current nodes are labeled, auto-scaling groups may create nodes lacking
// the label. Retain this behavior to support upgrades of Tectonic clusters
// created at 1.6.
func (k *Kontroller) legacyLabeler() {
	klog.V(6).Infof("Starting Flatcar Container Linux node auto-labeler")

	nodelist, err := k.nc.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.Infof("Failed listing nodes %v", err)

		return
	}

	// Match nodes that don't have an update-agent label.
	nodesMissingLabel := k8sutil.FilterNodesByRequirement(nodelist.Items, updateAgentLabelMissing)
	// Match nodes that identify as Flatcar Container Linux.
	nodesToLabel := k8sutil.FilterContainerLinuxNodes(nodesMissingLabel)

	klog.V(6).Infof("Found Flatcar Container Linux nodes to label: %+v", nodelist.Items)

	for _, node := range nodesToLabel {
		klog.Infof("Setting label 'agent=true' on %q", node.Name)

		if err := k8sutil.SetNodeLabels(k.nc, node.Name, enableUpdateAgentLabel); err != nil {
			klog.Errorf("Failed setting label 'agent=true' on %q", node.Name)
		}
	}
}

// runDaemonsetUpdate updates the agent on nodes if necessary.
//
// NOTE: the version for the agent is assumed to match the versioning scheme
// for the operator, thus our version is used to figure out the appropriate
// agent version.
// Furthermore, it's assumed that all future agent versions will be backwards
// compatible, so if the agent's version is greater than ours, it's okay.
func (k *Kontroller) runDaemonsetUpdate(agentImageRepo string) error {
	agentDaemonsets, err := k.kc.AppsV1().DaemonSets(k.namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set(managedByOperatorLabels)).String(),
	})
	if err != nil {
		return fmt.Errorf("listing DaemonSets: %w", err)
	}

	if len(agentDaemonsets.Items) == 0 {
		// No daemonset, create it.
		if err := k.createAgentDamonset(agentImageRepo); err != nil {
			return fmt.Errorf("creating agent DaemonSet: %w", err)
		}
		// runAgent succeeded, all should be well and converging now
		return nil
	}

	// There should only be one daemonset since we use a well-known name and
	// patch it each time rather than creating new ones.
	if len(agentDaemonsets.Items) > 1 {
		klog.Errorf("only expected one daemonset managed by operator; found %+v", agentDaemonsets.Items)

		return fmt.Errorf("only expected one daemonset managed by operator; found %v", len(agentDaemonsets.Items))
	}

	agentDS := agentDaemonsets.Items[0]

	var dsSemver semver.Version

	if dsVersion, ok := agentDS.Annotations[constants.AgentVersion]; ok {
		ver, err := semver.Parse(dsVersion)
		if err != nil {
			return fmt.Errorf("agent daemonset had version annotation, but it was not valid semver: %v[%v] = %v", agentDS.Name, constants.AgentVersion, dsVersion)
		}

		dsSemver = ver
	} else {
		klog.Errorf("managed daemonset did not have a version annotation: %+v", agentDS)

		return fmt.Errorf("managed daemonset did not have a version annotation")
	}

	if dsSemver.LT(version.Semver) {
		// Daemonset is too old, update it.
		//
		// TODO: perform a proper rolling update rather than delete-then-recreate
		// Right now, daemonset rolling updates aren't upstream and are thus fairly
		// painful to do correctly. In addition, doing it correctly doesn't add too
		// much value unless we have corresponding detection/rollback logic.
		falseVal := false

		err := k.kc.AppsV1().DaemonSets(k.namespace).Delete(context.TODO(), agentDS.Name, metav1.DeleteOptions{
			OrphanDependents: &falseVal, // Cascading delete.
		})
		if err != nil {
			klog.Errorf("could not delete old daemonset %+v: %v", agentDS, err)

			return fmt.Errorf("deleting old DaemonSet: %w", err)
		}

		err = k.createAgentDamonset(agentImageRepo)
		if err != nil {
			klog.Errorf("could not create new daemonset: %v", err)

			return fmt.Errorf("creating agent DaemonSet: %w", err)
		}
	}

	return nil
}

func (k *Kontroller) createAgentDamonset(agentImageRepo string) error {
	dsc := k.kc.AppsV1().DaemonSets(k.namespace)

	_, err := dsc.Create(context.TODO(), agentDaemonsetSpec(agentImageRepo), metav1.CreateOptions{})

	return err //nolint:wrapcheck
}

//nolint:funlen
func agentDaemonsetSpec(repo string) *appsv1.DaemonSet {
	// Each agent daemonset includes the version of the agent in the selector.
	// This ensures that the 'orphan adoption' logic doesn't kick in for these
	// daemonsets.
	versionedSelector := make(map[string]string)
	for k, v := range managedByOperatorLabels {
		versionedSelector[k] = v
	}

	versionedSelector[constants.AgentVersion] = version.Version

	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:   daemonsetName,
			Labels: managedByOperatorLabels,
			Annotations: map[string]string{
				constants.AgentVersion: version.Version,
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: versionedSelector},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:   agentDefaultAppName,
					Labels: versionedSelector,
					Annotations: map[string]string{
						constants.AgentVersion: version.Version,
					},
				},
				Spec: corev1.PodSpec{
					// Update the master nodes too.
					Tolerations: []corev1.Toleration{
						{
							Key:      "node-role.kubernetes.io/master",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "update-agent",
							Image:   agentImageName(repo),
							Command: agentCommand(),
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "var-run-dbus",
									MountPath: "/var/run/dbus",
								},
								{
									Name:      "etc-flatcar",
									MountPath: "/etc/flatcar",
								},
								{
									Name:      "usr-share-flatcar",
									MountPath: "/usr/share/flatcar",
								},
								{
									Name:      "etc-os-release",
									MountPath: "/etc/os-release",
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "UPDATE_AGENT_NODE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
								{
									Name: "POD_NAMESPACE",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.namespace",
										},
									},
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "var-run-dbus",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/var/run/dbus",
								},
							},
						},
						{
							Name: "etc-flatcar",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/etc/flatcar",
								},
							},
						},
						{
							Name: "usr-share-flatcar",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/usr/share/flatcar",
								},
							},
						},
						{
							Name: "etc-os-release",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/etc/os-release",
								},
							},
						},
					},
				},
			},
		},
	}
}

func agentImageName(repo string) string {
	return fmt.Sprintf("%s:v%s", repo, version.Version)
}

func agentCommand() []string {
	return []string{"/bin/update-agent"}
}
