/*
Copyright 2024 The Dapr Authors
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package processor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"text/template"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/dapr/components-contrib/secretstores"
	"github.com/dapr/dapr/pkg/apis/common"
	componentsapi "github.com/dapr/dapr/pkg/apis/components/v1alpha1"
	"github.com/dapr/dapr/pkg/components"
	diag "github.com/dapr/dapr/pkg/diagnostics"
	operatorv1 "github.com/dapr/dapr/pkg/proto/operator/v1"
	rterrors "github.com/dapr/dapr/pkg/runtime/errors"
	"github.com/dapr/kit/ptr"
)

// Init initializes a component of a category and reports the result.
func (p *Processor) Init(ctx context.Context, comp componentsapi.Component) error {
	initerr := p.init(ctx, comp)

	// after performing the initialization, report the result
	condition := operatorv1.ResourceConditionStatus_STATUS_SUCCESS
	var reason, message *string
	if initerr != nil {
		condition = operatorv1.ResourceConditionStatus_STATUS_FAILURE
		reason = ptr.Of("ERROR")
		message = ptr.Of(initerr.Error())
	}

	if err := p.reporter(ctx, comp,
		&operatorv1.ResourceResult{
			ResourceType:        operatorv1.ResourceType_RESOURCE_COMPONENT,
			EventType:           operatorv1.EventType_EVENT_INIT,
			Name:                comp.GetName(),
			Condition:           condition,
			Reason:              reason,
			Message:             message,
			ObservedGeneration:  comp.GetGeneration(),
			LastTransactionTime: timestamppb.New(time.Now()),
		}); err != nil {
		return errors.Join(initerr, fmt.Errorf("error reporting component init result: %w", err), p.Close(comp))
	}

	return initerr
}

// init initializes a component of a category.
func (p *Processor) init(ctx context.Context, comp componentsapi.Component) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	m, err := p.managerFromComp(comp)
	if err != nil {
		return err
	}

	if err := p.compStore.AddPendingComponentForCommit(comp); err != nil {
		return err
	}

	if err := m.Init(p.security.WithSVIDContext(ctx), comp); err != nil {
		return errors.Join(err, p.compStore.DropPendingComponent())
	}

	if err := p.compStore.CommitPendingComponent(); err != nil {
		return fmt.Errorf("error committing component: %w", err)
	}

	return nil
}

// Close closes the component and reports the result.
func (p *Processor) Close(comp componentsapi.Component) error {
	closeErr := p.internalClose(comp)

	// after performing the initialization, report the result
	condition := operatorv1.ResourceConditionStatus_STATUS_SUCCESS
	var reason, message *string
	if closeErr != nil {
		condition = operatorv1.ResourceConditionStatus_STATUS_FAILURE
		reason = ptr.Of("ERROR")
		message = ptr.Of(closeErr.Error())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.reporter(ctx, comp,
		&operatorv1.ResourceResult{
			ResourceType:        operatorv1.ResourceType_RESOURCE_COMPONENT,
			EventType:           operatorv1.EventType_EVENT_CLOSE,
			Name:                comp.GetName(),
			Condition:           condition,
			Reason:              reason,
			Message:             message,
			ObservedGeneration:  comp.GetGeneration(),
			LastTransactionTime: timestamppb.New(time.Now()),
		}); err != nil {
		return errors.Join(closeErr, fmt.Errorf("error reporting component close result: %w", err))
	}

	return closeErr
}

// internalClose closes the component.
func (p *Processor) internalClose(comp componentsapi.Component) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	m, err := p.managerFromComp(comp)
	if err != nil {
		return err
	}

	if err := m.Close(comp); err != nil {
		return err
	}

	p.compStore.DeleteComponent(comp.Name)

	return nil
}

func (p *Processor) AddPendingComponent(ctx context.Context, comp componentsapi.Component) bool {
	p.chlock.RLock()
	defer p.chlock.RUnlock()

	if p.shutdown.Load() {
		return false
	}

	p.pendingComponentsWaiting.RLock()

	select {
	case <-ctx.Done():
		p.pendingComponentsWaiting.RUnlock()
		return false
	case <-p.closedCh:
		p.pendingComponentsWaiting.RUnlock()
		return false
	case p.pendingComponents <- comp:
		return true
	}
}

func (p *Processor) processComponents(ctx context.Context) error {
	process := func(comp componentsapi.Component) error {
		if comp.Name == "" {
			return nil
		}

		err := p.processComponentAndDependents(ctx, comp)
		if err != nil {
			err = fmt.Errorf("process component %s error: %s", comp.Name, err)
			if !comp.Spec.IgnoreErrors {
				log.Warnf("Error processing component, daprd will exit gracefully: %s", err)
				return err
			}
			log.Errorf("Ignoring error processing component: %s", err)
		}
		return nil
	}

	for comp := range p.pendingComponents {
		err := process(comp)
		p.pendingComponentsWaiting.RUnlock()
		if err != nil {
			return err
		}
	}

	return nil
}

// WaitForEmptyComponentQueue waits for the component queue to be empty.
func (p *Processor) WaitForEmptyComponentQueue() {
	p.pendingComponentsWaiting.Lock()
	defer p.pendingComponentsWaiting.Unlock()
}

func (p *Processor) processComponentAndDependents(ctx context.Context, comp componentsapi.Component) error {
	log.Debug("Loading component: " + comp.LogName())
	res := p.preprocessOneComponent(ctx, &comp)
	if res.unreadyDependency != "" {
		p.pendingComponentDependents[res.unreadyDependency] = append(p.pendingComponentDependents[res.unreadyDependency], comp)
		return nil
	}

	compCategory := p.category(comp)
	if compCategory == "" {
		// the category entered is incorrect, return error
		return fmt.Errorf("incorrect type %s", comp.Spec.Type)
	}

	timeout, err := time.ParseDuration(comp.Spec.InitTimeout)
	if err != nil {
		timeout = defaultComponentInitTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err = p.Init(ctx, comp)
	// If the context is canceled, we want  to return an init error.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("init timeout for component %s exceeded after %s", comp.LogName(), timeout.String())
	}
	if err != nil {
		log.Errorf("Failed to init component %s: %s", comp.LogName(), err)
		diag.DefaultMonitoring.ComponentInitFailed(comp.Spec.Type, "init", comp.ObjectMeta.Name)
		return rterrors.NewInit(rterrors.InitComponentFailure, comp.LogName(), err)
	}

	log.Info("Component loaded: " + comp.LogName())
	diag.DefaultMonitoring.ComponentLoaded()

	dependency := componentDependency(compCategory, comp.Name)
	if deps, ok := p.pendingComponentDependents[dependency]; ok {
		delete(p.pendingComponentDependents, dependency)
		for _, dependent := range deps {
			if err := p.processComponentAndDependents(ctx, dependent); err != nil {
				return err
			}
		}
	}

	return nil
}

type componentPreprocessRes struct {
	unreadyDependency string
}

func (p *Processor) preprocessOneComponent(ctx context.Context, comp *componentsapi.Component) componentPreprocessRes {
	// Existing secret processing logic
	_, unreadySecretsStore := p.secret.ProcessResource(ctx, comp)
	if unreadySecretsStore != "" {
		return componentPreprocessRes{
			unreadyDependency: componentDependency(components.CategorySecretStore, unreadySecretsStore),
		}
	}

	// New Template Rendering Logic
	for i, meta := range comp.Spec.Metadata {
		if len(meta.TemplateRefs) > 0 && meta.Value.String() != "" {
			// Resolve template variables
			resolvedValues, err := p.resolveTemplateRefs(ctx, comp.GetNamespace(), meta.TemplateRefs)
			if err != nil {
				log.Errorf("failed to resolve template refs for '%s': %v", meta.Name, err)
				// Decide if you want to fail hard or just log the error
				continue
			}

			// Parse and render the template
			tmpl, err := template.New(meta.Name).Parse(meta.Value.String())
			if err != nil {
				log.Errorf("failed to parse template for '%s': %v", meta.Name, err)
				continue
			}

			var renderedValue bytes.Buffer
			if err := tmpl.Execute(&renderedValue, resolvedValues); err != nil {
				log.Errorf("failed to render template for '%s': %v", meta.Name, err)
				continue
			}

			// Update the metadata value with the rendered template
			comp.Spec.Metadata[i].Value = common.DynamicValue{
				JSON: apiextensionsv1.JSON{
					Raw: []byte(`"` + renderedValue.String() + `"`),
				},
			}
		}
	}

	return componentPreprocessRes{}
}

func (p *Processor) category(comp componentsapi.Component) components.Category {
	for category := range p.managers {
		if strings.HasPrefix(comp.Spec.Type, string(category)+".") {
			return category
		}
	}
	return ""
}

func componentDependency(compCategory components.Category, name string) string {
	return string(compCategory) + ":" + name
}

func (p *Processor) resolveTemplateRefs(ctx context.Context, namespace string, refs map[string]common.TemplateRef) (map[string]string, error) {
	resolved := make(map[string]string)
	for key, ref := range refs {
		if ref.Value != nil {
			resolved[key] = *ref.Value
		} else if ref.SecretKeyRef != nil {
			secretStore, ok := p.compStore.GetSecretStore(ref.SecretKeyRef.Name)
			if !ok {
				return nil, fmt.Errorf("secret store '%s' not found", ref.SecretKeyRef.Name)
			}
			// Passing the namespace here is important for Kubernetes secret store
			val, err := secretStore.GetSecret(ctx, secretstores.GetSecretRequest{
				Name: ref.SecretKeyRef.Key,
				Metadata: map[string]string{
					"namespace": namespace,
				},
			})
			if err != nil {
				return nil, fmt.Errorf("failed to get secret '%s' from secret store '%s': %w", ref.SecretKeyRef.Key, ref.SecretKeyRef.Name, err)
			}
			resolved[key] = val.Data[ref.SecretKeyRef.Key]
		}
	}
	return resolved, nil
}
