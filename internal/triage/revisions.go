package triage

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// revisionAnnotation is set by the deployment controller on every ReplicaSet
// it owns. it is the only durable record of which template was which revision.
const revisionAnnotation = "deployment.kubernetes.io/revision"

// RevisionDiffInput selects the deployment and the two revisions to compare.
type RevisionDiffInput struct {
	Namespace string `json:"namespace" jsonschema:"namespace of the deployment"`
	Name      string `json:"name" jsonschema:"name of the deployment"`
	// both revisions are optional. the default is the question people
	// actually ask: what changed in the latest rollout.
	FromRevision int64 `json:"fromRevision,omitempty" jsonschema:"older revision number; defaults to the revision before toRevision"`
	ToRevision   int64 `json:"toRevision,omitempty" jsonschema:"newer revision number; defaults to the latest"`
}

// RevisionInfo summarises one revision's ReplicaSet.
type RevisionInfo struct {
	Revision   int64  `json:"revision" jsonschema:"deployment revision number"`
	ReplicaSet string `json:"replicaSet" jsonschema:"ReplicaSet holding this revision's template"`
	Created    string `json:"created,omitempty" jsonschema:"RFC3339 time the ReplicaSet was created"`
	Replicas   int32  `json:"replicas" jsonschema:"current replicas in this ReplicaSet"`
	Ready      int32  `json:"readyReplicas" jsonschema:"ready replicas in this ReplicaSet"`
	// ChangeCause is the kubernetes.io/change-cause annotation, when set.
	ChangeCause string `json:"changeCause,omitempty" jsonschema:"recorded reason for the rollout, if any"`
}

// SpecChange is one field-level difference between two pod templates.
type SpecChange struct {
	// Category groups changes for the agent: image, env, resources, probe,
	// command, volume or other.
	Category  string `json:"category" jsonschema:"image, env, resources, probe, command, volume or other"`
	Container string `json:"container,omitempty" jsonschema:"container the change applies to, when container scoped"`
	// Field is a readable path, e.g. env.DATABASE_URL or resources.limits.memory.
	Field string `json:"field" jsonschema:"what changed, e.g. env.DATABASE_URL or livenessProbe.initialDelaySeconds"`
	// Change is added, removed or modified.
	Change string `json:"change" jsonschema:"added, removed or modified"`
	From   string `json:"from,omitempty" jsonschema:"value in the older revision"`
	To     string `json:"to,omitempty" jsonschema:"value in the newer revision"`
}

// RevisionDiffOutput is the structured "what changed between deploys" answer.
type RevisionDiffOutput struct {
	Object ObjectRef `json:"object" jsonschema:"the deployment compared"`
	// From is always the older revision, regardless of argument order: a
	// reversed diff would invert the meaning of every change.
	From RevisionInfo `json:"from" jsonschema:"older revision"`
	To   RevisionInfo `json:"to" jsonschema:"newer revision"`
	// Available lists every revision still on record, so the agent can ask
	// for a different pair without guessing.
	Available []int64      `json:"availableRevisions" jsonschema:"revisions still retained by the deployment"`
	Changes   []SpecChange `json:"changes" jsonschema:"field-level differences, older to newer"`
	Findings  []Finding    `json:"findings" jsonschema:"one finding per category of change"`
}

// DiffDeploymentRevisions compares the pod templates of two revisions.
func DiffDeploymentRevisions(ctx context.Context, clientset kubernetes.Interface, in RevisionDiffInput) (RevisionDiffOutput, error) {
	if err := requireName("namespace", in.Namespace); err != nil {
		return RevisionDiffOutput{}, err
	}
	if err := requireName("name", in.Name); err != nil {
		return RevisionDiffOutput{}, err
	}
	deployment, err := clientset.AppsV1().Deployments(in.Namespace).Get(ctx, in.Name, metav1.GetOptions{})
	if err != nil {
		return RevisionDiffOutput{}, fmt.Errorf("getting deployment %s/%s: %w", in.Namespace, in.Name, err)
	}

	revisions, err := ownedRevisions(ctx, clientset, deployment)
	if err != nil {
		return RevisionDiffOutput{}, err
	}
	available := make([]int64, 0, len(revisions))
	for revision := range revisions {
		available = append(available, revision)
	}
	sort.Slice(available, func(i, j int) bool { return available[i] < available[j] })

	if len(available) < 2 && (in.FromRevision == 0 || in.ToRevision == 0) {
		return RevisionDiffOutput{}, fmt.Errorf(
			"deployment %s/%s has %d retained revision(s) %v; at least two are needed to diff. "+
				"revisionHistoryLimit may be pruning older ReplicaSets",
			in.Namespace, in.Name, len(available), available)
	}

	from, to, err := pickRevisions(in.FromRevision, in.ToRevision, available)
	if err != nil {
		return RevisionDiffOutput{}, fmt.Errorf("deployment %s/%s: %w", in.Namespace, in.Name, err)
	}

	older, newer := revisions[from], revisions[to]
	changes := diffPodTemplates(older.Spec.Template.Spec, newer.Spec.Template.Spec)

	return RevisionDiffOutput{
		Object:    ObjectRef{Kind: "Deployment", Namespace: deployment.Namespace, Name: deployment.Name},
		From:      revisionInfo(from, older),
		To:        revisionInfo(to, newer),
		Available: available,
		Changes:   changes,
		Findings:  changeFindings(changes),
	}, nil
}

// ownedRevisions maps revision number to ReplicaSet for a deployment.
func ownedRevisions(ctx context.Context, clientset kubernetes.Interface, deployment *appsv1.Deployment) (map[int64]*appsv1.ReplicaSet, error) {
	if deployment.Spec.Selector == nil {
		return nil, fmt.Errorf("deployment %s/%s has no selector", deployment.Namespace, deployment.Name)
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("parsing selector of deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}

	list, err := clientset.AppsV1().ReplicaSets(deployment.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, fmt.Errorf("listing replicasets of deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}

	revisions := map[int64]*appsv1.ReplicaSet{}
	for index := range list.Items {
		replicaSet := &list.Items[index]
		// the selector alone is not enough: another deployment with
		// overlapping labels could own ReplicaSets that also match. the
		// owner reference is authoritative.
		if !ownedBy(replicaSet, deployment) {
			continue
		}
		revision, parseErr := strconv.ParseInt(replicaSet.Annotations[revisionAnnotation], 10, 64)
		if parseErr != nil {
			continue
		}
		revisions[revision] = replicaSet
	}
	return revisions, nil
}

func ownedBy(replicaSet *appsv1.ReplicaSet, deployment *appsv1.Deployment) bool {
	for _, owner := range replicaSet.OwnerReferences {
		if owner.Kind != "Deployment" || owner.Name != deployment.Name {
			continue
		}
		// a uid match is exact. the fake clientset and some test fixtures
		// leave uids empty, so fall back to kind and name in that case.
		if owner.UID == "" || deployment.UID == "" || owner.UID == deployment.UID {
			return true
		}
	}
	return false
}

// pickRevisions resolves defaults and normalises order so from is older.
func pickRevisions(requestedFrom, requestedTo int64, available []int64) (int64, int64, error) {
	exists := func(revision int64) bool {
		for _, candidate := range available {
			if candidate == revision {
				return true
			}
		}
		return false
	}

	to := requestedTo
	if to == 0 {
		to = available[len(available)-1]
	}
	if !exists(to) {
		return 0, 0, fmt.Errorf("revision %d not found; available revisions are %v", to, available)
	}

	from := requestedFrom
	if from == 0 {
		// the revision immediately before `to` among those retained. numbers
		// can have gaps, because a rollback re-labels an old ReplicaSet with
		// a new revision number instead of creating a new one.
		for index := len(available) - 1; index >= 0; index-- {
			if available[index] < to {
				from = available[index]
				break
			}
		}
		if from == 0 {
			return 0, 0, fmt.Errorf("no revision older than %d is retained; available revisions are %v", to, available)
		}
	}
	if !exists(from) {
		return 0, 0, fmt.Errorf("revision %d not found; available revisions are %v", from, available)
	}
	if from == to {
		return 0, 0, fmt.Errorf("fromRevision and toRevision are both %d; pick two different revisions from %v", from, available)
	}
	if from > to {
		from, to = to, from
	}
	return from, to, nil
}

func revisionInfo(revision int64, replicaSet *appsv1.ReplicaSet) RevisionInfo {
	return RevisionInfo{
		Revision:    revision,
		ReplicaSet:  replicaSet.Name,
		Created:     formatTime(replicaSet.CreationTimestamp),
		Replicas:    replicaSet.Status.Replicas,
		Ready:       replicaSet.Status.ReadyReplicas,
		ChangeCause: replicaSet.Annotations["kubernetes.io/change-cause"],
	}
}

// diffPodTemplates compares two pod specs. it deep-diffs six areas that cause
// most deploy regressions (image, env, resources, probes, command and args,
// volumes) and reports anything else as an "other" change with its field name,
// without pretending to understand every PodSpec field.
func diffPodTemplates(older, newer corev1.PodSpec) []SpecChange {
	changes := []SpecChange{}
	changes = append(changes, diffContainerLists(older.InitContainers, newer.InitContainers, true)...)
	changes = append(changes, diffContainerLists(older.Containers, newer.Containers, false)...)
	changes = append(changes, diffVolumes(older.Volumes, newer.Volumes)...)
	changes = append(changes, diffOtherPodFields(older, newer)...)
	return changes
}

func diffContainerLists(older, newer []corev1.Container, init bool) []SpecChange {
	var changes []SpecChange
	prefix := ""
	if init {
		prefix = "initContainer"
	}
	olderByName := map[string]corev1.Container{}
	for _, container := range older {
		olderByName[container.Name] = container
	}
	newerByName := map[string]corev1.Container{}
	for _, container := range newer {
		newerByName[container.Name] = container
	}

	for _, container := range newer {
		previous, found := olderByName[container.Name]
		if !found {
			changes = append(changes, SpecChange{
				Category: "other", Container: container.Name,
				Field: joinField(prefix, "container"), Change: "added", To: container.Image,
			})
			continue
		}
		changes = append(changes, diffContainer(previous, container)...)
	}
	for _, container := range older {
		if _, found := newerByName[container.Name]; !found {
			changes = append(changes, SpecChange{
				Category: "other", Container: container.Name,
				Field: joinField(prefix, "container"), Change: "removed", From: container.Image,
			})
		}
	}
	return changes
}

func diffContainer(older, newer corev1.Container) []SpecChange {
	var changes []SpecChange
	name := newer.Name
	add := func(category, field, from, to string) {
		if from == to {
			return
		}
		change := "modified"
		switch {
		case from == "":
			change = "added"
		case to == "":
			change = "removed"
		}
		changes = append(changes, SpecChange{
			Category: category, Container: name, Field: field, Change: change, From: from, To: to,
		})
	}

	add("image", "image", older.Image, newer.Image)
	add("command", "command", joinArgs(older.Command), joinArgs(newer.Command))
	add("command", "args", joinArgs(older.Args), joinArgs(newer.Args))
	add("command", "workingDir", older.WorkingDir, newer.WorkingDir)

	// env: compared by variable name on the raw rendering, then redacted for
	// display. comparing before redacting matters: two different passwords
	// both render as [REDACTED], and comparing those would hide a real change.
	olderEnv := envMap(older.Env)
	newerEnv := envMap(newer.Env)
	for _, key := range sortedEnvKeys(olderEnv, newerEnv) {
		from, to := olderEnv[key], newerEnv[key]
		if from.raw == to.raw {
			continue
		}
		change := "modified"
		switch {
		case from.raw == "":
			change = "added"
		case to.raw == "":
			change = "removed"
		}
		changes = append(changes, SpecChange{
			Category: "env", Container: name, Field: "env." + key, Change: change,
			From: from.display(key), To: to.display(key),
		})
	}
	add("env", "envFrom", describeEnvFrom(older.EnvFrom), describeEnvFrom(newer.EnvFrom))

	olderResources := resourceMap(older.Resources)
	newerResources := resourceMap(newer.Resources)
	for _, key := range sortedKeys(olderResources, newerResources) {
		add("resources", "resources."+key, olderResources[key], newerResources[key])
	}

	for _, probe := range []struct {
		name         string
		older, newer *corev1.Probe
	}{
		{"livenessProbe", older.LivenessProbe, newer.LivenessProbe},
		{"readinessProbe", older.ReadinessProbe, newer.ReadinessProbe},
		{"startupProbe", older.StartupProbe, newer.StartupProbe},
	} {
		olderProbe := probeMap(probe.older)
		newerProbe := probeMap(probe.newer)
		for _, key := range sortedKeys(olderProbe, newerProbe) {
			add("probe", probe.name+"."+key, olderProbe[key], newerProbe[key])
		}
	}

	olderMounts := mountMap(older.VolumeMounts)
	newerMounts := mountMap(newer.VolumeMounts)
	for _, key := range sortedKeys(olderMounts, newerMounts) {
		add("volume", "volumeMounts."+key, olderMounts[key], newerMounts[key])
	}

	// container fields outside the six deep-diffed areas: name them, do not
	// interpret them.
	for _, field := range []struct {
		name         string
		older, newer any
	}{
		{"ports", older.Ports, newer.Ports},
		{"securityContext", older.SecurityContext, newer.SecurityContext},
		{"lifecycle", older.Lifecycle, newer.Lifecycle},
		{"imagePullPolicy", older.ImagePullPolicy, newer.ImagePullPolicy},
	} {
		if !reflect.DeepEqual(field.older, field.newer) {
			changes = append(changes, SpecChange{Category: "other", Container: name, Field: field.name, Change: "modified"})
		}
	}
	return changes
}

func diffVolumes(older, newer []corev1.Volume) []SpecChange {
	describe := func(volumes []corev1.Volume) map[string]string {
		result := map[string]string{}
		for _, volume := range volumes {
			result[volume.Name] = describeVolumeSource(volume.VolumeSource)
		}
		return result
	}
	olderVolumes, newerVolumes := describe(older), describe(newer)
	var changes []SpecChange
	for _, key := range sortedKeys(olderVolumes, newerVolumes) {
		from, to := olderVolumes[key], newerVolumes[key]
		if from == to {
			continue
		}
		change := "modified"
		if from == "" {
			change = "added"
		} else if to == "" {
			change = "removed"
		}
		changes = append(changes, SpecChange{Category: "volume", Field: "volumes." + key, Change: change, From: from, To: to})
	}
	return changes
}

func diffOtherPodFields(older, newer corev1.PodSpec) []SpecChange {
	var changes []SpecChange
	for _, field := range []struct {
		name         string
		older, newer any
	}{
		{"serviceAccountName", older.ServiceAccountName, newer.ServiceAccountName},
		{"nodeSelector", older.NodeSelector, newer.NodeSelector},
		{"affinity", older.Affinity, newer.Affinity},
		{"tolerations", older.Tolerations, newer.Tolerations},
		{"securityContext", older.SecurityContext, newer.SecurityContext},
		{"hostNetwork", older.HostNetwork, newer.HostNetwork},
		{"dnsPolicy", older.DNSPolicy, newer.DNSPolicy},
		{"terminationGracePeriodSeconds", older.TerminationGracePeriodSeconds, newer.TerminationGracePeriodSeconds},
		{"imagePullSecrets", older.ImagePullSecrets, newer.ImagePullSecrets},
		{"priorityClassName", older.PriorityClassName, newer.PriorityClassName},
		{"topologySpreadConstraints", older.TopologySpreadConstraints, newer.TopologySpreadConstraints},
	} {
		if !reflect.DeepEqual(field.older, field.newer) {
			changes = append(changes, SpecChange{Category: "other", Field: "pod." + field.name, Change: "modified"})
		}
	}
	return changes
}

// envEntry is one environment variable rendered for comparison.
type envEntry struct {
	// raw is the comparable form: a literal value, or a description of where
	// a reference points. it is never shown when the value is a literal.
	raw string
	// literal is true for inline values, which are the only ones that can
	// hold secret material; references describe a source, not a value.
	literal bool
}

// display returns the form safe to send to the model. literal values pass
// through redaction; reference descriptions are shown as-is, because they
// contain no secret material even when the variable is named PASSWORD.
func (entry envEntry) display(name string) string {
	if !entry.literal {
		return entry.raw
	}
	return redactValue(name, entry.raw)
}

// envMap renders each variable for comparison. references show where the
// value comes from (source and key), never the referenced value. this never
// reads a Secret or ConfigMap, and cannot, because the server's RBAC grants no
// access to them.
func envMap(env []corev1.EnvVar) map[string]envEntry {
	result := map[string]envEntry{}
	for _, variable := range env {
		result[variable.Name] = describeEnvValue(variable)
	}
	return result
}

func sortedEnvKeys(maps ...map[string]envEntry) []string {
	set := map[string]bool{}
	for _, m := range maps {
		for key := range m {
			set[key] = true
		}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func describeEnvValue(variable corev1.EnvVar) envEntry {
	source := variable.ValueFrom
	if source == nil {
		// an explicitly empty value must still differ from "not set".
		if variable.Value == "" {
			return envEntry{raw: `""`, literal: true}
		}
		return envEntry{raw: variable.Value, literal: true}
	}
	switch {
	case source.SecretKeyRef != nil:
		return envEntry{raw: fmt.Sprintf("secretKeyRef(%s/%s)", source.SecretKeyRef.Name, source.SecretKeyRef.Key)}
	case source.ConfigMapKeyRef != nil:
		return envEntry{raw: fmt.Sprintf("configMapKeyRef(%s/%s)", source.ConfigMapKeyRef.Name, source.ConfigMapKeyRef.Key)}
	case source.FieldRef != nil:
		return envEntry{raw: fmt.Sprintf("fieldRef(%s)", source.FieldRef.FieldPath)}
	case source.ResourceFieldRef != nil:
		return envEntry{raw: fmt.Sprintf("resourceFieldRef(%s)", source.ResourceFieldRef.Resource)}
	}
	return envEntry{raw: "valueFrom(other)"}
}

func describeEnvFrom(sources []corev1.EnvFromSource) string {
	var parts []string
	for _, source := range sources {
		switch {
		case source.SecretRef != nil:
			parts = append(parts, "secret:"+source.SecretRef.Name)
		case source.ConfigMapRef != nil:
			parts = append(parts, "configMap:"+source.ConfigMapRef.Name)
		}
	}
	return strings.Join(parts, ",")
}

func resourceMap(requirements corev1.ResourceRequirements) map[string]string {
	result := map[string]string{}
	for name, quantity := range requirements.Requests {
		result["requests."+string(name)] = quantity.String()
	}
	for name, quantity := range requirements.Limits {
		result["limits."+string(name)] = quantity.String()
	}
	return result
}

func probeMap(probe *corev1.Probe) map[string]string {
	result := map[string]string{}
	if probe == nil {
		return result
	}
	handler := probe.ProbeHandler
	switch {
	case handler.HTTPGet != nil:
		result["httpGet"] = fmt.Sprintf("%s:%s", handler.HTTPGet.Path, handler.HTTPGet.Port.String())
	case handler.TCPSocket != nil:
		result["tcpSocket"] = handler.TCPSocket.Port.String()
	case handler.Exec != nil:
		result["exec"] = joinArgs(handler.Exec.Command)
	case handler.GRPC != nil:
		result["grpc"] = strconv.Itoa(int(handler.GRPC.Port))
	}
	// zero values are kubernetes defaults that the API server fills in, so a
	// field left unset and the same field set to its default compare equal
	// only after defaulting. report what is in the template.
	setInt := func(key string, value int32) {
		if value != 0 {
			result[key] = strconv.Itoa(int(value))
		}
	}
	setInt("initialDelaySeconds", probe.InitialDelaySeconds)
	setInt("timeoutSeconds", probe.TimeoutSeconds)
	setInt("periodSeconds", probe.PeriodSeconds)
	setInt("successThreshold", probe.SuccessThreshold)
	setInt("failureThreshold", probe.FailureThreshold)
	return result
}

func mountMap(mounts []corev1.VolumeMount) map[string]string {
	result := map[string]string{}
	for _, mount := range mounts {
		value := mount.Name
		if mount.ReadOnly {
			value += " (readOnly)"
		}
		if mount.SubPath != "" {
			value += " subPath=" + mount.SubPath
		}
		result[mount.MountPath] = value
	}
	return result
}

func describeVolumeSource(source corev1.VolumeSource) string {
	switch {
	case source.Secret != nil:
		return "secret:" + source.Secret.SecretName
	case source.ConfigMap != nil:
		return "configMap:" + source.ConfigMap.Name
	case source.PersistentVolumeClaim != nil:
		return "pvc:" + source.PersistentVolumeClaim.ClaimName
	case source.EmptyDir != nil:
		return "emptyDir"
	case source.HostPath != nil:
		return "hostPath:" + source.HostPath.Path
	case source.Projected != nil:
		return "projected"
	case source.DownwardAPI != nil:
		return "downwardAPI"
	}
	return "other"
}

// changeFindings emits one finding per category, listing the fields. the full
// detail is already in Changes, so a finding per field would only repeat it.
func changeFindings(changes []SpecChange) []Finding {
	categoryCodes := []struct{ category, code string }{
		{"image", CodeImageChanged},
		{"env", CodeEnvChanged},
		{"resources", CodeResourcesChanged},
		{"probe", CodeProbeChanged},
		{"command", CodeCommandChanged},
		{"volume", CodeOtherSpecChanged},
		{"other", CodeOtherSpecChanged},
	}
	grouped := map[string][]string{}
	for _, change := range changes {
		field := change.Field
		if change.Container != "" {
			field = change.Container + ":" + field
		}
		grouped[change.Category] = append(grouped[change.Category], field)
	}

	findings := []Finding{}
	emitted := map[string]bool{}
	for _, entry := range categoryCodes {
		fields := grouped[entry.category]
		if len(fields) == 0 {
			continue
		}
		// volume and other share a code; merge them into one finding.
		if emitted[entry.code] {
			for index := range findings {
				if findings[index].Code == entry.code {
					merged := append(findings[index].Evidence["fields"].([]string), fields...)
					findings[index] = findings[index].WithEvidence("fields", merged)
				}
			}
			continue
		}
		emitted[entry.code] = true
		findings = append(findings, NewFinding(entry.code, SeverityInfo,
			entry.category+" changed between revisions").
			WithEvidence("fields", fields))
	}
	return findings
}

func sortedKeys(maps ...map[string]string) []string {
	set := map[string]bool{}
	for _, m := range maps {
		for key := range m {
			set[key] = true
		}
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func joinArgs(args []string) string {
	if len(args) == 0 {
		return ""
	}
	quoted := make([]string, len(args))
	for index, arg := range args {
		quoted[index] = strconv.Quote(arg)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func joinField(prefix, field string) string {
	if prefix == "" {
		return field
	}
	return prefix + "." + field
}
