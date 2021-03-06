package env

import (
	"bytes"
	"context"
	"fmt"
	api "github.com/cockroachlabs/crdb-operator/api/v1alpha1"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/yaml"
	"sort"
	"strings"
	"sync"
	"testing"
)

const (
	DefaultNsName = "crdb-test-"
)

func NewSandbox(t *testing.T, env *ActiveEnv) Sandbox {
	ns := DefaultNsName + rand.String(6)

	mgr, err := ctrl.NewManager(env.k8s.Cfg, ctrl.Options{
		Scheme:             env.scheme,
		Namespace:          ns,
		MetricsBindAddress: "0", // disable metrics serving
	})
	if err != nil {
		t.Fatal(err)
	}

	s := Sandbox{
		env:       env,
		Namespace: ns,
		Mgr:       mgr,
	}

	if err := createNamespace(s); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(s.Cleanup)

	return s
}

type Sandbox struct {
	env *ActiveEnv
	Mgr ctrl.Manager

	Namespace string
}

type namespaceCreatable interface {
	RuntimeObject(string) apiruntime.Object
}

func (s Sandbox) Create(nc namespaceCreatable) error {
	return s.env.Create(context.TODO(), nc.RuntimeObject(s.Namespace))
}

func (s Sandbox) Get(o apiruntime.Object) error {
	accessor, err := meta.Accessor(o)
	if err != nil {
		return err
	}

	key := types.NamespacedName{
		Namespace: s.Namespace,
		Name:      accessor.GetName(),
	}

	return s.env.Get(context.TODO(), key, o)
}

func (s Sandbox) Cleanup() {
	dp := metav1.DeletePropagationForeground
	opts := &metav1.DeleteOptions{PropagationPolicy: &dp}
	nss := s.env.Clientset.CoreV1().Namespaces()
	if err := nss.Delete(s.Namespace, opts); err != nil {
		fmt.Println("failed to cleanup namespace", s.Namespace)
	}
}

func (s Sandbox) StartManager(t *testing.T, maker func(ctrl.Manager) error) {
	if err := maker(s.Mgr); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(startCtrlMgr(t, s.Mgr))
}

func createNamespace(s Sandbox) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: s.Namespace,
		},
	}

	if _, err := s.env.Clientset.CoreV1().Namespaces().Create(ns); err != nil {
		return errors.Wrapf(err, "failed to create namespace: %s", s.Namespace)
	}

	return nil
}

func startCtrlMgr(t *testing.T, mgr manager.Manager) func() {
	stop := make(chan struct{})
	wg := &sync.WaitGroup{}
	wg.Add(1)

	go func() {
		defer wg.Done()
		if err := mgr.Start(stop); err != nil {
			t.Fatal(err)
		}
	}()

	return func() {
		close(stop)
		wg.Wait()
	}
}

func NewDiffingSandbox(t *testing.T, env *ActiveEnv) DiffingSandbox {
	s := NewSandbox(t, env)

	return DiffingSandbox{
		Sandbox:      s,
		originalObjs: listAllObjsOrDie(s),
	}
}

type DiffingSandbox struct {
	Sandbox

	originalObjs objList
}

func (ds *DiffingSandbox) Diff() (string, error) {
	oo, err := listAllObjs(ds.Sandbox)
	if err != nil {
		return "", err
	}

	diff := oo.Diff(ds.originalObjs)

	redacted := strings.ReplaceAll(diff, ds.Namespace, "[sandbox_namespace]")

	return redacted, nil
}

func listAllObjs(s Sandbox) (objList, error) {
	var l objList
	for _, gvr := range s.env.resources {
		res := s.env.namespaceableResource(gvr)

		list, err := res.Namespace(s.Namespace).List(metav1.ListOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, "failed to list objects in namespace %s", s.Namespace)
		}

		l = append(l, list.Items...)
	}

	sort.Sort(l)

	return l, nil
}

func listAllObjsOrDie(s Sandbox) objList {
	oo, err := listAllObjs(s)
	if err != nil {
		panic(err)
	}

	return oo
}

type objList []unstructured.Unstructured

func (l objList) Diff(other objList) string {
	diff := objList{}

OUTER:
	for _, o1 := range l {
		for _, o2 := range other {
			if o1.GroupVersionKind() == o2.GroupVersionKind() && o1.GetName() == o2.GetName() {
				continue OUTER
			}
		}

		diff = append(diff, o1)
	}

	return string(diff.ToYamlOrDie())
}

func (l objList) ToYamlOrDie() []byte {
	var out bytes.Buffer
	for _, u := range l {
		filterUnnecessary(&u)

		bs, err := yaml.Marshal(u.Object)
		if err != nil {
			panic(err)
		}
		out.WriteString("---\n")
		out.Write(bs)
		out.WriteRune('\n')
	}

	return out.Bytes()
}

func (l objList) Len() int      { return len(l) }
func (l objList) Swap(i, j int) { l[i], l[j] = l[j], l[i] }
func (l objList) Less(i, j int) bool {
	a, b := &l[i], &l[j]

	if a.GetNamespace() != b.GetNamespace() {
		return a.GetNamespace() < b.GetNamespace()
	}

	agvk, bgvk := a.GroupVersionKind(), b.GroupVersionKind()

	if agvk.Group != bgvk.Group {
		if agvk.Group == api.GroupVersion.Group {
			return true
		}

		if bgvk.Group == api.GroupVersion.Group {
			return false
		}
	}

	if a.GetKind() != b.GetKind() {
		return a.GetKind() < b.GetKind()
	}

	return a.GetName() < b.GetName()
}

func filterUnnecessary(u *unstructured.Unstructured) {
	if u.GetKind() == "Pod" && u.GetAPIVersion() == "v1" {
		unstructured.RemoveNestedField(u.Object, "spec", "nodeName")
		unstructured.RemoveNestedField(u.Object, "spec", "hostname")

		replaceDefaultTokenNames(u)
		filterPodLabels(u)
	}

	if u.GetKind() == "Service" && u.GetAPIVersion() == "v1" {
		replaceServiceIP(u)
	}

	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "metadata", "generation")
	unstructured.RemoveNestedField(u.Object, "metadata", "namespace")
	unstructured.RemoveNestedField(u.Object, "metadata", "resourceVersion")
	unstructured.RemoveNestedField(u.Object, "metadata", "selfLink")
	unstructured.RemoveNestedField(u.Object, "metadata", "uid")
	unstructured.RemoveNestedField(u.Object, "metadata", "generateName")
	unstructured.RemoveNestedField(u.Object, "metadata", "ownerReferences")
	unstructured.RemoveNestedField(u.Object, "status")
}

func replaceDefaultTokenNames(u *unstructured.Unstructured) {
	containers, ok, err := unstructured.NestedSlice(u.Object, "spec", "containers")
	if (err != nil) || !ok {
		return
	}

	replacements := make(map[string]string)

	var newContainers []interface{}
	for i, rawContainer := range containers {
		container, _ := rawContainer.(map[string]interface{})

		volumeMounts, ok, _ := unstructured.NestedSlice(container, "volumeMounts")
		if ok {
			var newVolumeMounts []interface{}
			for _, rawVolumeMount := range volumeMounts {
				volumeMount, _ := rawVolumeMount.(map[string]interface{})

				vmName, _ := volumeMount["name"].(string)

				if strings.HasPrefix(vmName, "default-token-") {
					newName := fmt.Sprintf("default-token-%d", i)
					replacements[vmName] = newName
					volumeMount["name"] = newName
				}

				newVolumeMounts = append(newVolumeMounts, volumeMount)
			}

			_ = unstructured.SetNestedSlice(container, newVolumeMounts, "volumeMounts")
		}

		newContainers = append(newContainers, container)
	}

	_ = unstructured.SetNestedSlice(u.Object, newContainers, "spec", "containers")

	volumes, ok, err := unstructured.NestedSlice(u.Object, "spec", "volumes")
	var newVolumes []interface{}
	if (err == nil) && ok {
		for _, rawVolume := range volumes {
			volume, mapOk := rawVolume.(map[string]interface{})
			name, nameOk := volume["name"].(string)
			secretMap, secretOk, _ := unstructured.NestedMap(volume, "secret")
			if mapOk && nameOk && secretOk {
				_, replaced := replacements[name]
				_, hasSecretName := secretMap["secretName"]
				if replaced && hasSecretName {
					volume["name"] = replacements[name]
					secretMap["secretName"] = replacements[name]

					_ = unstructured.SetNestedMap(volume, secretMap, "secret")
				}
			}

			newVolumes = append(newVolumes, volume)
		}
	}

	_ = unstructured.SetNestedSlice(u.Object, newVolumes, "spec", "volumes")
}

func filterPodLabels(u *unstructured.Unstructured) {
	labels := u.GetLabels()
	delete(labels, "controller-revision-hash")
	u.SetLabels(labels)
}

func replaceServiceIP(u *unstructured.Unstructured) {
	clusterIP, ok, err := unstructured.NestedString(u.Object, "spec", "clusterIP")
	if (err != nil) || !ok {
		return
	}

	if clusterIP != "None" {
		_ = unstructured.SetNestedField(u.Object, "[some_ip]", "spec", "clusterIP")
	}
}
