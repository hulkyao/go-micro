package kubernetes

import (
	"github.com/micro/go-micro/v3/logger"
	"github.com/micro/go-micro/v3/runtime"
	"github.com/micro/go-micro/v3/util/kubernetes/client"
)

// createNamespace creates a namespace resource
func (k *kubernetes) createNamespace(namespace *runtime.Namespace) error {
	err := k.client.Create(&client.Resource{
		Kind: "namespace",
		Value: client.Namespace{
			Metadata: &client.Metadata{
				Name: namespace.Name,
			},
		},
	})
	if err != nil {
		if logger.V(logger.ErrorLevel, logger.DefaultLogger) {
			logger.Errorf("Error creating namespace %s: %v", namespace.ID(), err)
		}
	}
	return err
}

// deleteNamespace deletes a namespace resource
func (k *kubernetes) deleteNamespace(namespace *runtime.Namespace) error {
	err := k.client.Delete(&client.Resource{
		Kind: "namespace",
		Name: namespace.Name,
	})
	if err != nil {
		if err != nil {
			if logger.V(logger.ErrorLevel, logger.DefaultLogger) {
				logger.Errorf("Error deleting namespace %s: %v", namespace.ID(), err)
			}
		}
	}
	return err
}
