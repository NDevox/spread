package deploy

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	kube "k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/meta"
	rest "k8s.io/kubernetes/pkg/client/restclient"
	kubecli "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/client/unversioned/clientcmd"
	"k8s.io/kubernetes/pkg/kubectl/cmd/config"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/strategicpatch"
)

// DefaultContext is the current kubectl context and is represented with an empty string.
const DefaultContext = ""

// KubeCluster is able to deploy to Kubernetes clusters. This is a very simple implementation with no error recovery.
type KubeCluster struct {
	Client    *kubecli.Client
	context   string
	localkube bool
}

// NewKubeClusterFromContext creates a KubeCluster using a Kubernetes client with the configuration of the given context.
// If the context name is empty, the default context will be used.
func NewKubeClusterFromContext(name string) (*KubeCluster, error) {
	rulesConfig, err := defaultLoadingRules().Load()
	if err != nil {
		return nil, fmt.Errorf("could not load rules: %v", err)
	}

	clientConfig := clientcmd.NewNonInteractiveClientConfig(*rulesConfig, name, &clientcmd.ConfigOverrides{})

	rawConfig, err := clientConfig.RawConfig()
	if err != nil || rawConfig.Contexts == nil {
		return nil, fmt.Errorf("could not access kubectl config: %v", err)
	}

	if name == DefaultContext {
		name = rawConfig.CurrentContext
	}

	if rawConfig.Contexts[name] == nil {
		return nil, fmt.Errorf("context '%s' does not exist", name)
	}

	restClientConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("could not get ClientConfig: %v", err)
	}

	client, err := kubecli.New(restClientConfig)
	if err != nil {
		return nil, fmt.Errorf("could not create Kubernetes client: %v", err)
	}

	return &KubeCluster{
		Client:    client,
		context:   name,
		localkube: name == "localkube",
	}, nil
}

// Context returns the kubectl context being used
func (c *KubeCluster) Context() string {
	return c.context
}

// Deploy creates/updates the Deployment's objects on the Kubernetes cluster.
// Currently no error recovery is implemented; if there is an error the deployment process will immediately halt and return the error.
// If update is not set, will error if objects exist. If deleteModifiedPods is set, pods of modified RCs will be deleted.
func (c *KubeCluster) Deploy(dep *Deployment, update, deleteModifiedPods bool) error {
	if c.Client == nil {
		return errors.New("client not setup (was nil)")
	}

	// create namespaces before everything else
	for _, nsObj := range dep.ObjectsOfVersionKind("", "Namespace") {
		ns := nsObj.(*kube.Namespace)
		_, err := c.Client.Namespaces().Create(ns)
		if err != nil && !alreadyExists(err) {
			return err
		}
	}

	// TODO: add continue on error and error lists
	for _, obj := range dep.Objects() {
		// don't create namespaces again
		if _, isNamespace := obj.(*kube.Namespace); isNamespace {
			continue
		}

		err := c.deploy(obj, update)
		if err != nil {
			return err
		}

		if rc, isRC := obj.(*kube.ReplicationController); isRC && deleteModifiedPods {
			err = c.deletePods(rc)
			if err != nil {
				return fmt.Errorf("could not delete pods for rc `%s/%s`: %v", rc.Namespace, rc.Name, err)
			}
		}
	}

	printLoadBalancers(c.Client, dep.ObjectsOfVersionKind("", "Service"), c.localkube)

	// deployed successfully
	return nil
}

// deploy creates the object on the connected Kubernetes instance. Errors if object exists and not updating.
func (c *KubeCluster) deploy(obj KubeObject, update bool) error {
	if obj == nil {
		return errors.New("tried to deploy nil object")
	}

	mapping, err := mapping(obj)
	if err != nil {
		return err
	}

	if update {
		_, err := c.update(obj, true, mapping)
		if err != nil {
			return err
		}
		return nil
	}

	_, err = c.create(obj, mapping)
	return err
}

// update replaces the currently deployed version with a new one. If the objects already match then nothing is done.
func (c *KubeCluster) update(obj KubeObject, create bool, mapping *meta.RESTMapping) (KubeObject, error) {
	meta := obj.GetObjectMeta()

	deployed, err := c.get(meta.GetNamespace(), meta.GetName(), true, mapping)
	if doesNotExist(err) && create {
		return c.create(obj, mapping)
	} else if err != nil {
		return nil, err
	}

	// TODO: need a better way to handle resource versioning
	// set resource version on local to same as remote
	deployedVersion := deployed.GetObjectMeta().GetResourceVersion()
	meta.SetResourceVersion(deployedVersion)

	copyImmutables(deployed, obj)

	// if local matches deployed, do nothing
	if kube.Semantic.DeepEqual(obj, deployed) {
		return deployed, nil
	}

	patch, err := diff(deployed, obj)
	if err != nil {
		return nil, fmt.Errorf("could not create diff: %v", err)
	}

	req := c.Client.RESTClient.Patch(kube.StrategicMergePatchType).
		Name(meta.GetName()).
		Body(patch)

	setRequestObjectInfo(req, meta.GetNamespace(), mapping)

	runtimeObj, err := req.Do().Get()
	if err != nil {
		return nil, resourceError("update", meta.GetNamespace(), meta.GetName(), mapping, err)
	}

	return AsKubeObject(runtimeObj)
}

// Get retrieves an objects from a cluster using it's namespace name and API version.
func (c *KubeCluster) Get(kind, namespace, name string, export bool) (KubeObject, error) {
	kind = KubeShortForm(kind)

	req := c.Client.Get().Resource(kind).Namespace(namespace).Name(name)

	if export {
		req.Param("export", "true")
	}

	runObj, err := req.Do().Get()
	if err != nil {
		return nil, fmt.Errorf("Failed to retrieve resource '%s/%s (namespace=%s)' from Kube API server: %v", kind, name, namespace, err)
	}

	kubeObj, err := AsKubeObject(runObj)
	if err != nil {
		return nil, fmt.Errorf("Unable to change into KubeObject: %v", err)
	}

	return kubeObj, nil
}

// get retrieves the object from the cluster.
func (c *KubeCluster) get(namespace, name string, export bool, mapping *meta.RESTMapping) (KubeObject, error) {
	req := c.Client.RESTClient.Get().Name(name)
	setRequestObjectInfo(req, namespace, mapping)

	if export {
		req.Param("export", "true")
	}

	runtimeObj, err := req.Do().Get()
	if err != nil {
		return nil, resourceError("get", namespace, name, mapping, err)
	}

	return AsKubeObject(runtimeObj)
}

// create adds the object to the cluster.
func (c *KubeCluster) create(obj KubeObject, mapping *meta.RESTMapping) (KubeObject, error) {
	meta := obj.GetObjectMeta()
	req := c.Client.RESTClient.Post().Body(obj)

	setRequestObjectInfo(req, meta.GetNamespace(), mapping)

	runtimeObj, err := req.Do().Get()
	if err != nil {
		return nil, resourceError("create", meta.GetName(), meta.GetNamespace(), mapping, err)
	}

	return AsKubeObject(runtimeObj)
}

func (c *KubeCluster) deletePods(rc *kube.ReplicationController) error {
	if rc == nil {
		return errors.New("rc was nil")
	}

	// list pods
	opts := kube.ListOptions{
		LabelSelector: labels.Set(rc.Spec.Selector).AsSelector(),
	}
	podList, err := c.Client.Pods(rc.Namespace).List(opts)
	if err != nil {
		return err
	}

	// delete pods
	for _, pod := range podList.Items {
		err := c.Client.Pods(pod.Namespace).Delete(pod.Name, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *KubeCluster) Deployment() (*Deployment, error) {
	deployment := new(Deployment)
	for _, resource := range resources {
		obj, err := c.Client.Get().Resource(resource).Do().Get()
		if err != nil {
			return nil, fmt.Errorf("could not list '%s': %v", resource, err)
		}

		// TODO: this is what desperation looks like
		switch t := obj.(type) {
		case *kube.ComponentStatusList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.ConfigMapList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.EndpointsList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.LimitRangeList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.NamespaceList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.PersistentVolumeClaimList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.PersistentVolumeList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.PodList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.ReplicationControllerList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.ResourceQuotaList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.SecretList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.ServiceAccountList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		case *kube.ServiceList:
			for _, item := range t.Items {
				if err := deployment.Add(&item); err != nil {
					return nil, err
				}
			}
		default:
			return nil, fmt.Errorf("could not match '%T' to type", obj)
		}

	}
	return deployment, nil
}

// setRequestObjectInfo adds necessary type information to requests.
func setRequestObjectInfo(req *rest.Request, namespace string, mapping *meta.RESTMapping) {
	// if namespace scoped resource, set namespace
	req.NamespaceIfScoped(namespace, isNamespaceScoped(mapping))

	// set resource name
	req.Resource(mapping.Resource)
}

// alreadyExists checks if the error is for a resource already existing.
func alreadyExists(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasSuffix(err.Error(), "already exists")
}

// doesNotExist checks if the error is for a non-existent resource.
func doesNotExist(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasSuffix(err.Error(), "not found")
}

// mapping returns the appropriate RESTMapping for the object.
func mapping(obj KubeObject) (*meta.RESTMapping, error) {
	gvk, err := kube.Scheme.ObjectKind(obj)
	if err != nil {
		return nil, err
	}

	mapping, err := kube.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("could not create RESTMapping for %s: %v", gvk, err)
	}
	return mapping, nil
}

// isNamespaceScoped returns if the mapping is scoped by Namespace.
func isNamespaceScoped(mapping *meta.RESTMapping) bool {
	return mapping.Scope.Name() == meta.RESTScopeNameNamespace
}

// defaultLoadingRules use the same rules (as of 2/17/16) as kubectl.
func defaultLoadingRules() *clientcmd.ClientConfigLoadingRules {
	opts := config.NewDefaultPathOptions()

	loadingRules := opts.LoadingRules
	loadingRules.Precedence = opts.GetLoadingPrecedence()
	return loadingRules
}

// diff creates a patch.
func diff(original, modified runtime.Object) (patch []byte, err error) {
	origBytes, err := json.Marshal(original)
	if err != nil {
		return nil, err
	}

	modBytes, err := json.Marshal(modified)
	if err != nil {
		return nil, err
	}

	return strategicpatch.CreateTwoWayMergePatch(origBytes, modBytes, original)
}

// asKubeObject attempts use the object as a KubeObject. It will return an error if not possible.
func AsKubeObject(runtimeObj runtime.Object) (KubeObject, error) {
	kubeObj, ok := runtimeObj.(KubeObject)
	if !ok {
		return nil, errors.New("was unable to use runtime.Object as deploy.KubeObject")
	}
	return kubeObj, nil
}

func resourceError(action, namespace, name string, mapping *meta.RESTMapping, err error) error {
	if mapping == nil || mapping.GroupVersionKind.IsEmpty() {
		return fmt.Errorf("could not %s '%s/%s': %v", action, namespace, name, err)
	}
	gvk := mapping.GroupVersionKind
	return fmt.Errorf("could not %s '%s/%s' (%s): %v", action, namespace, name, gvk.Kind, err)
}

// copyImmutables sets any immutable fields from src on dst. Will panic if objects not of same type.
func copyImmutables(src, dst KubeObject) {
	if src == nil || dst == nil {
		return
	}

	// each type has specific fields that must be copied
	switch src := src.(type) {
	case *kube.Service:
		dst := dst.(*kube.Service)
		dst.Spec.ClusterIP = src.Spec.ClusterIP
	}
}

// printLoadBalancers blocks until all Services of type LoadBalancer have been deployed, printing it's details as it becomes available.
// Will panic if given something other than services
func printLoadBalancers(client *kubecli.Client, services []KubeObject, localkube bool) {
	if len(services) == 0 {
		return
	}

	first := true
	completed := map[string]bool{}

	// checks when we've seen every service
	done := func() bool {
		for _, svcObj := range services {
			s := svcObj.(*kube.Service)
			if s.Spec.Type == kube.ServiceTypeLoadBalancer && !completed[s.Name] {
				return false
			}
		}
		return true
	}

	for {
		if done() {
			return
		}

		if first {
			fmt.Println("Waiting for load balancer deployment...")
			first = false
		}

		for _, svcObj := range services {
			s := svcObj.(*kube.Service)
			if s.Spec.Type == kube.ServiceTypeLoadBalancer && !completed[s.Name] {
				clusterVers, err := client.Services(s.Namespace).Get(s.Name)
				if err != nil {
					fmt.Printf("Error getting service `%s`: %v\n", s.Name, err)
				}

				if localkube {
					completed[s.Name] = true
					for _, port := range clusterVers.Spec.Ports {
						fmt.Printf("'%s/%s' - %s available on localkube host port:\t %d\n", s.Namespace, s.Name, port.Name, port.NodePort)
					}
				}

				loadBalancers := clusterVers.Status.LoadBalancer.Ingress
				for _, lb := range loadBalancers {
					completed[s.Name] = true

					host := lb.Hostname
					if len(lb.IP) != 0 {
						if len(host) == 0 {
							host = lb.IP
						} else {
							host += fmt.Sprintf(" (%s)", lb.IP)
						}
					}
					fmt.Printf("Service '%s/%s' available at: \t%s\n", s.Namespace, s.Name, host)
				}
			}
		}

		// prevents warning about throttling
		time.Sleep(250 * time.Millisecond)
	}
}
