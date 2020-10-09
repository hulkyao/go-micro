// Package kubernetes implements kubernetes micro runtime
package kubernetes

import (
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/micro/go-micro/v3/logger"
	log "github.com/micro/go-micro/v3/logger"
	"github.com/micro/go-micro/v3/runtime"
	"github.com/micro/go-micro/v3/util/kubernetes/api"
	"github.com/micro/go-micro/v3/util/kubernetes/client"
)

// action to take on runtime service
type action int

type kubernetes struct {
	sync.RWMutex
	// options configure runtime
	options runtime.Options
	// indicates if we're running
	running bool
	// client is kubernetes client
	client client.Client
	// namespaces which exist
	namespaces []client.Namespace
}

// namespaceExists returns a boolean indicating if a namespace exists
func (k *kubernetes) namespaceExists(name string) (bool, error) {
	// populate the cache
	if k.namespaces == nil {
		if logger.V(logger.DebugLevel, logger.DefaultLogger) {
			logger.Debugf("Populating namespace cache")
		}

		namespaceList := new(client.NamespaceList)
		resource := &client.Resource{Kind: "namespace", Value: namespaceList}
		if err := k.client.List(resource); err != nil {
			return false, err
		}

		if logger.V(logger.DebugLevel, logger.DefaultLogger) {
			logger.Debugf("Popualted namespace cache successfully with %v items", len(namespaceList.Items))
		}
		k.namespaces = namespaceList.Items
	}

	// check if the namespace exists in the cache
	for _, n := range k.namespaces {
		if n.Metadata.Name == name {
			return true, nil
		}
	}

	return false, nil
}

// autoCreateNamespace creates a new k8s namespace
func (k *kubernetes) autoCreateNamespace(namespace string) error {
	ns := client.Namespace{Metadata: &client.Metadata{Name: namespace}}
	err := k.client.Create(&client.Resource{Kind: "namespace", Value: ns}, client.CreateNamespace(namespace))

	// ignore err already exists
	if err != nil && strings.Contains(err.Error(), "already exists") {
		logger.Debugf("Ignoring ErrAlreadyExists for namespace %v: %v", namespace, err)
		err = nil
	}

	// add to cache and create networkpolicy
	if err == nil && k.namespaces != nil {
		k.namespaces = append(k.namespaces, ns)

		if networkPolicy, err := runtime.NewNetworkPolicy("ingress", namespace, map[string]string{"owner": "micro"}); err != nil {
			return err
		} else {
			return k.Create(networkPolicy, runtime.CreateNamespace(namespace))
		}
	}

	return err
}

// getService queries kubernetes for micro service
// NOTE: this function is not thread-safe
func (k *kubernetes) getService(labels map[string]string, opts ...client.GetOption) ([]*service, error) {
	// get the service status
	serviceList := new(client.ServiceList)
	r := &client.Resource{
		Kind:  "service",
		Value: serviceList,
	}

	opts = append(opts, client.GetLabels(labels))

	// get the service from k8s
	if err := k.client.Get(r, opts...); err != nil {
		return nil, err
	}

	// get the deployment status
	depList := new(client.DeploymentList)
	d := &client.Resource{
		Kind:  "deployment",
		Value: depList,
	}
	if err := k.client.Get(d, opts...); err != nil {
		return nil, err
	}

	// get the pods from k8s
	podList := new(client.PodList)
	p := &client.Resource{
		Kind:  "pod",
		Value: podList,
	}
	if err := k.client.Get(p, opts...); err != nil {
		return nil, err
	}

	// service map
	svcMap := make(map[string]*service)

	// collect info from kubernetes service
	for _, kservice := range serviceList.Items {
		// name of the service
		name := kservice.Metadata.Labels["name"]
		// version of the service
		version := kservice.Metadata.Labels["version"]

		srv := &service{
			Service: &runtime.Service{
				Name:     name,
				Version:  version,
				Metadata: make(map[string]string),
			},
			kservice: &kservice,
		}

		// set the address
		address := kservice.Spec.ClusterIP
		port := kservice.Spec.Ports[0]
		srv.Service.Metadata["address"] = fmt.Sprintf("%s:%d", address, port.Port)
		// set the type of service
		srv.Service.Metadata["type"] = kservice.Metadata.Labels["micro"]

		// copy annotations metadata into service metadata
		for k, v := range kservice.Metadata.Annotations {
			srv.Service.Metadata[k] = v
		}

		// save as service
		svcMap[name+version] = srv
	}

	// collect additional info from kubernetes deployment
	for _, kdep := range depList.Items {
		// name of the service
		name := kdep.Metadata.Labels["name"]
		// versio of the service
		version := kdep.Metadata.Labels["version"]

		// access existing service map based on name + version
		if svc, ok := svcMap[name+version]; ok {
			// we're expecting our own service name in metadata
			if _, ok := kdep.Metadata.Annotations["name"]; !ok {
				continue
			}

			// set the service name, version and source
			// based on existing annotations we stored
			svc.Service.Name = kdep.Metadata.Annotations["name"]
			svc.Service.Version = kdep.Metadata.Annotations["version"]
			svc.Service.Source = kdep.Metadata.Annotations["source"]

			// delete from metadata
			delete(kdep.Metadata.Annotations, "name")
			delete(kdep.Metadata.Annotations, "version")
			delete(kdep.Metadata.Annotations, "source")

			// copy all annotations metadata into service metadata
			for k, v := range kdep.Metadata.Annotations {
				svc.Service.Metadata[k] = v
			}

			// parse out deployment status and inject into service metadata
			if len(kdep.Status.Conditions) > 0 {
				status := transformStatus(kdep.Status.Conditions[0].Type)
				svc.Status(status, nil)
				svc.Metadata["started"] = kdep.Status.Conditions[0].LastUpdateTime
			} else {
				svc.Status(runtime.Unknown, nil)
			}

			// get the real status
			for _, item := range podList.Items {
				// check the name
				if item.Metadata.Labels["name"] != name {
					continue
				}
				// check the version
				if item.Metadata.Labels["version"] != version {
					continue
				}

				status := transformStatus(item.Status.Phase)

				// skip if we can't get the container
				if len(item.Status.Containers) == 0 {
					continue
				}

				// now try get a deeper status
				state := item.Status.Containers[0].State
				// set start time
				if state.Running != nil {
					svc.Metadata["started"] = state.Running.Started
				}

				// set status from waiting
				if v := state.Waiting; v != nil {
					status = runtime.Starting
				}

				svc.Status(status, nil)
			}

			// save deployment
			svc.kdeploy = &kdep
		}
	}

	// collect all the services and return
	services := make([]*service, 0, len(serviceList.Items))

	for _, service := range svcMap {
		services = append(services, service)
	}

	return services, nil
}

// Init initializes runtime options
func (k *kubernetes) Init(opts ...runtime.Option) error {
	k.Lock()
	defer k.Unlock()

	for _, o := range opts {
		o(&k.options)
	}

	return nil
}

func (k *kubernetes) Logs(resource runtime.Resource, options ...runtime.LogsOption) (runtime.Logs, error) {

	// Handle the various different types of resources:
	switch resource.Type() {
	case runtime.TypeNamespace:
		// noop (Namespace is not supported by *kubernetes.Logs())
		return nil, nil
	case runtime.TypeNetworkPolicy:
		// noop (NetworkPolicy is not supported by *kubernetes.Logs()))
		return nil, nil
	case runtime.TypeService:

		// Assert the resource back into a *runtime.Service
		s, ok := resource.(*runtime.Service)
		if !ok {
			return nil, runtime.ErrInvalidResource
		}

		klo := newLog(k.client, s.Name, options...)

		if !klo.options.Stream {
			records, err := klo.Read()
			if err != nil {
				log.Errorf("Failed to get logs for service '%v' from k8s: %v", s.Name, err)
				return nil, err
			}
			kstream := &kubeStream{
				stream: make(chan runtime.Log),
				stop:   make(chan bool),
			}
			go func() {
				for _, record := range records {
					kstream.Chan() <- record
				}
				kstream.Stop()
			}()
			return kstream, nil
		}
		stream, err := klo.Stream()
		if err != nil {
			return nil, err
		}
		return stream, nil
	default:
		return nil, runtime.ErrInvalidResource
	}
}

type kubeStream struct {
	// the k8s log stream
	stream chan runtime.Log
	// the stop chan
	sync.Mutex
	stop chan bool
	err  error
}

func (k *kubeStream) Error() error {
	return k.err
}

func (k *kubeStream) Chan() chan runtime.Log {
	return k.stream
}

func (k *kubeStream) Stop() error {
	k.Lock()
	defer k.Unlock()
	select {
	case <-k.stop:
		return nil
	default:
		close(k.stop)
		close(k.stream)
	}
	return nil
}

// Create a resource
func (k *kubernetes) Create(resource runtime.Resource, opts ...runtime.CreateOption) error {
	k.Lock()
	defer k.Unlock()

	options := runtime.CreateOptions{
		Type:      k.options.Type,
		Namespace: client.DefaultNamespace,
	}
	for _, o := range opts {
		o(&options)
	}

	// Handle the various different types of resources:
	switch resource.Type() {
	case runtime.TypeNamespace:
		// Assert the resource back into a *runtime.Namespace
		namespace, ok := resource.(*runtime.Namespace)
		if !ok {
			return runtime.ErrInvalidResource
		}
		return k.createNamespace(namespace)
	case runtime.TypeNetworkPolicy:
		// Assert the resource back into a *runtime.NetworkPolicy
		networkPolicy, ok := resource.(*runtime.NetworkPolicy)
		if !ok {
			return runtime.ErrInvalidResource
		}
		return k.createNetworkPolicy(networkPolicy)
	case runtime.TypeService:

		// Assert the resource back into a *runtime.Service
		s, ok := resource.(*runtime.Service)
		if !ok {
			return runtime.ErrInvalidResource
		}

		// default type if it doesn't exist
		if len(options.Type) == 0 {
			options.Type = k.options.Type
		}

		// default the source if it doesn't exist
		if len(s.Source) == 0 {
			s.Source = k.options.Source
		}

		// ensure the namespace exists
		namespace := client.SerializeResourceName(options.Namespace)
		// only do this if the namespace is not default
		if namespace != "default" {
			if exist, err := k.namespaceExists(namespace); err == nil && !exist {
				if err := k.autoCreateNamespace(namespace); err != nil {
					if logger.V(logger.WarnLevel, logger.DefaultLogger) {
						logger.Warnf("Error creating namespace %v: %v", namespace, err)
					}
					return err
				}
			} else if err != nil {
				if logger.V(logger.WarnLevel, logger.DefaultLogger) {
					logger.Warnf("Error checking namespace %v exists: %v", namespace, err)
				}
				return err
			}
		}
		// determine the image from the source and options
		options.Image = k.getImage(s, options)

		// create a secret for the credentials if some where provided
		if len(options.Secrets) > 0 {
			if err := k.createCredentials(s, options); err != nil {
				if logger.V(logger.WarnLevel, logger.DefaultLogger) {
					logger.Warnf("Error generating auth credentials for service: %v", err)
				}
				return err
			}

			if logger.V(logger.DebugLevel, logger.DefaultLogger) {
				logger.Debugf("Generated auth credentials for service %v", s.Name)
			}
		}

		// create new service
		service := newService(s, options)

		// start the service
		return service.Start(k.client, client.CreateNamespace(options.Namespace))
	default:
		return runtime.ErrInvalidResource
	}
}

// Read returns all instances of given service
func (k *kubernetes) Read(opts ...runtime.ReadOption) ([]*runtime.Service, error) {
	k.Lock()
	defer k.Unlock()

	// set the default labels
	labels := map[string]string{}

	options := runtime.ReadOptions{
		Namespace: client.DefaultNamespace,
	}

	for _, o := range opts {
		o(&options)
	}

	if len(options.Service) > 0 {
		labels["name"] = client.Format(options.Service)
	}

	// add version to labels if a version has been supplied
	if len(options.Version) > 0 {
		labels["version"] = client.Format(options.Version)
	}

	if len(options.Type) > 0 {
		labels["micro"] = options.Type
	}

	srvs, err := k.getService(labels, client.GetNamespace(options.Namespace))
	if err != nil {
		return nil, err
	}

	var services []*runtime.Service
	for _, service := range srvs {
		services = append(services, service.Service)
	}

	return services, nil
}

// Update a resource in place
func (k *kubernetes) Update(resource runtime.Resource, opts ...runtime.UpdateOption) error {
	options := runtime.UpdateOptions{
		Namespace: client.DefaultNamespace,
	}
	for _, o := range opts {
		o(&options)
	}

	// Handle the various different types of resources:
	switch resource.Type() {
	case runtime.TypeNamespace:
		// noop (Namespace is not supported by *kubernetes.Update())
		return nil
	case runtime.TypeNetworkPolicy:
		// Assert the resource back into a *runtime.NetworkPolicy
		networkPolicy, ok := resource.(*runtime.NetworkPolicy)
		if !ok {
			return runtime.ErrInvalidResource
		}
		return k.updateNetworkPolicy(networkPolicy)
	case runtime.TypeService:

		// Assert the resource back into a *runtime.Service
		s, ok := resource.(*runtime.Service)
		if !ok {
			return runtime.ErrInvalidResource
		}

		labels := map[string]string{}

		if len(s.Name) > 0 {
			labels["name"] = client.Format(s.Name)
		}

		if len(s.Version) > 0 {
			labels["version"] = client.Format(s.Version)
		}

		// get the existing service
		services, err := k.getService(labels, client.GetNamespace(options.Namespace))
		if err != nil {
			return err
		}

		// update the relevant services
		for _, service := range services {
			// nil check
			if service.kdeploy.Metadata == nil || service.kdeploy.Metadata.Annotations == nil {
				md := new(client.Metadata)
				md.Annotations = make(map[string]string)
				service.kdeploy.Metadata = md
			}

			// update metadata
			for k, v := range s.Metadata {
				service.kdeploy.Metadata.Annotations[k] = v
			}

			// update build time annotation
			service.kdeploy.Spec.Template.Metadata.Annotations["updated"] = fmt.Sprintf("%d", time.Now().Unix())

			// update the service
			if err := service.Update(k.client, client.UpdateNamespace(options.Namespace)); err != nil {
				return err
			}
		}

		return nil
	default:
		return runtime.ErrInvalidResource
	}
}

// Delete removes a resource
func (k *kubernetes) Delete(resource runtime.Resource, opts ...runtime.DeleteOption) error {
	k.Lock()
	defer k.Unlock()

	options := runtime.DeleteOptions{
		Namespace: client.DefaultNamespace,
	}
	for _, o := range opts {
		o(&options)
	}

	// Handle the various different types of resources:
	switch resource.Type() {
	case runtime.TypeNamespace:
		// Assert the resource back into a *runtime.Namespace
		namespace, ok := resource.(*runtime.Namespace)
		if !ok {
			return runtime.ErrInvalidResource
		}
		return k.deleteNamespace(namespace)
	case runtime.TypeNetworkPolicy:
		// Assert the resource back into a *runtime.NetworkPolicy
		networkPolicy, ok := resource.(*runtime.NetworkPolicy)
		if !ok {
			return runtime.ErrInvalidResource
		}
		return k.deleteNetworkPolicy(networkPolicy)
	case runtime.TypeService:

		// Assert the resource back into a *runtime.Service
		s, ok := resource.(*runtime.Service)
		if !ok {
			return runtime.ErrInvalidResource
		}

		// create new kubernetes micro service
		service := newService(s, runtime.CreateOptions{
			Type:      k.options.Type,
			Namespace: options.Namespace,
		})

		// delete the service credentials
		ns := client.DeleteNamespace(options.Namespace)
		k.client.Delete(&client.Resource{Name: credentialsName(s), Kind: "secret"}, ns)

		if err := service.Stop(k.client, ns); err == api.ErrNotFound {
			return runtime.ErrNotFound
		} else if err != nil {
			return err
		}

		return nil
	default:
		return runtime.ErrInvalidResource
	}
}

// Start starts the runtime
func (k *kubernetes) Start() error {
	k.Lock()
	defer k.Unlock()

	// already running
	if k.running {
		return nil
	}

	// set running
	k.running = true
	return nil
}

// Stop shuts down the runtime
func (k *kubernetes) Stop() error {
	k.Lock()
	defer k.Unlock()

	if !k.running {
		return nil
	}

	// set not running
	k.running = false
	return nil
}

// String implements stringer interface
func (k *kubernetes) String() string {
	return "kubernetes"
}

// NewRuntime creates new kubernetes runtime
func NewRuntime(opts ...runtime.Option) runtime.Runtime {
	// get default options
	options := runtime.Options{
		// Create labels with type "micro": "service"
		Type: "service",
	}

	// apply requested options
	for _, o := range opts {
		o(&options)
	}

	// kubernetes client
	client := client.NewClusterClient()

	return &kubernetes{
		options: options,
		client:  client,
	}
}

func (k *kubernetes) getImage(s *runtime.Service, options runtime.CreateOptions) string {
	// use the image when its specified
	if len(options.Image) > 0 {
		return options.Image
	}

	if len(k.options.Image) > 0 {
		return k.options.Image
	}

	return ""
}
func (k *kubernetes) createCredentials(service *runtime.Service, options runtime.CreateOptions) error {
	data := make(map[string]string, len(options.Secrets))
	for key, value := range options.Secrets {
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}

	// construct the k8s secret object
	secret := &client.Secret{
		Type: "Opaque",
		Data: data,
		Metadata: &client.Metadata{
			Name:      credentialsName(service),
			Namespace: options.Namespace,
		},
	}

	// crete the secret in kubernetes
	name := credentialsName(service)
	return k.client.Create(&client.Resource{
		Kind: "secret", Name: name, Value: secret,
	}, client.CreateNamespace(options.Namespace))
}

func credentialsName(service *runtime.Service) string {
	name := fmt.Sprintf("%v-%v-credentials", service.Name, service.Version)
	return client.SerializeResourceName(name)
}

// transformStatus takes a deployment status (deploymentcondition.type) and transforms it into a
// runtime service status, e.g. containercreating => starting
func transformStatus(depStatus string) runtime.ServiceStatus {
	switch strings.ToLower(depStatus) {
	case "pending":
		return runtime.Starting
	case "containercreating":
		return runtime.Starting
	case "imagepullbackoff":
		return runtime.Error
	case "crashloopbackoff":
		return runtime.Error
	case "error":
		return runtime.Error
	case "running":
		return runtime.Running
	case "available":
		return runtime.Running
	case "succeeded":
		return runtime.Stopped
	case "failed":
		return runtime.Error
	case "waiting":
		return runtime.Starting
	case "terminated":
		return runtime.Stopped
	default:
		return runtime.Unknown
	}
}
