/*
 Copyright 2022 The KubeVela Authors.

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

package sync

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/oam"

	"github.com/kubevela/velaux/pkg/server/domain/model"
	"github.com/kubevela/velaux/pkg/server/domain/service"
	"github.com/kubevela/velaux/pkg/server/infrastructure/datastore"
)

// getApp will return the app and appname if exists
func (c *CR2UX) getApp(ctx context.Context, name, namespace string) (*model.Application, string, error) {
	alreadyCreated := &model.Application{Name: formatAppComposedName(name, namespace)}
	err1 := c.ds.Get(ctx, alreadyCreated)
	if err1 == nil {
		return alreadyCreated, alreadyCreated.Name, nil
	}

	// check if it's created the first in database
	existApp := &model.Application{Name: name}
	err2 := c.ds.Get(ctx, existApp)
	if err2 == nil {
		en := existApp.Labels[model.LabelSyncNamespace]
		// it means the namespace/app is not created yet, the appname is occupied by app from other namespace
		if en != namespace {
			return nil, formatAppComposedName(name, namespace), err1
		}
		return existApp, name, nil
	}
	return nil, name, err2
}

// CR2UX provides the Add/Update/Delete method
type CR2UX struct {
	ds                 datastore.DataStore
	cli                client.Client
	cache              sync.Map
	userService        service.UserService
	projectService     service.ProjectService
	applicationService service.ApplicationService
	workflowService    service.WorkflowService
	targetService      service.TargetService
	envService         service.EnvService
}

func formatAppComposedName(name, namespace string) string {
	return name + "-" + namespace
}

// we need to prevent the case that one app is deleted ant it's name is pure appName, then other app with namespace suffix will be mixed
func (c *CR2UX) getAppMetaName(ctx context.Context, name, namespace string) string {
	_, appName, _ := c.getApp(ctx, name, namespace)
	return appName
}

func (c *CR2UX) syncAppCreatedByUX(ctx context.Context, targetApp *v1beta1.Application) error {
	appPrimaryKey := targetApp.Annotations[oam.AnnotationAppName]
	if appPrimaryKey == "" {
		return fmt.Errorf("appName is empty in application %s", targetApp.Name)
	}
	if targetApp.Annotations == nil || targetApp.Annotations[oam.AnnotationPublishVersion] == "" {
		klog.Warningf("app %s/%s has no publish version, skip sync workflow status", targetApp.Namespace, targetApp.Name)
	}
	recordName := targetApp.Annotations[oam.AnnotationPublishVersion]
	if err := c.workflowService.SyncWorkflowRecord(ctx, appPrimaryKey, recordName, targetApp, nil); err != nil {
		klog.ErrorS(err, "failed to sync workflow status", "oam app name", targetApp.Name, "workflow name", oam.GetPublishVersion(targetApp), "record name", recordName)
		return err
	}
	return nil
}

func (c *CR2UX) syncAppCreatedByCLI(ctx context.Context, targetApp *v1beta1.Application) error {
	if c.shouldSyncMetaFromCLI(ctx, targetApp, false) {
		ds := c.ds
		dsApp, err := c.ConvertApp2DatastoreApp(ctx, targetApp)
		if err != nil {
			klog.Errorf("Convert App to data store err %v", err)
			return err
		}
		if dsApp.Project != nil {
			if err = StoreProject(ctx, *dsApp.Project, ds, c.projectService, c.userService); err != nil {
				klog.Errorf("get or create project for sync process err %v", err)
				return err
			}
		}

		if err = StoreTargets(ctx, dsApp, ds, c.targetService); err != nil {
			klog.Errorf("Store targets to data store err %v", err)
			return err
		}

		if err = StoreEnv(ctx, dsApp, ds, c.envService); err != nil {
			klog.Errorf("Store Env Metadata to data store err %v", err)
			return err
		}
		if err = StoreEnvBinding(ctx, dsApp.Eb, ds); err != nil {
			klog.Errorf("Store EnvBinding Metadata to data store err %v", err)
			return err
		}
		if err = StoreComponents(ctx, dsApp.AppMeta.Name, dsApp.Comps, ds); err != nil {
			klog.Errorf("Store Components Metadata to data store err %v", err)
			return err
		}
		if err = StorePolicy(ctx, dsApp.AppMeta.Name, dsApp.Policies, ds); err != nil {
			klog.Errorf("Store Policy Metadata to data store err %v", err)
			return err
		}
		if err = StoreWorkflow(ctx, dsApp, ds); err != nil {
			klog.Errorf("Store Workflow Metadata to data store err %v", err)
			return err
		}

		if err = StoreApplicationRevision(ctx, dsApp, ds); err != nil {
			klog.Errorf("Store application revision to data store err %v", err)
			return err
		}

		if err = StoreWorkflowRecord(ctx, dsApp, ds); err != nil {
			klog.Errorf("Store Workflow Record to data store err %v", err)
			return err
		}

		if err = StoreAppMeta(ctx, dsApp, ds); err != nil {
			klog.Errorf("Store App Metadata to data store err %v", err)
			return err
		}

		// update cache
		key := formatAppComposedName(targetApp.Name, targetApp.Namespace)
		syncedVersion := getSyncedRevision(dsApp.Revision)
		c.syncCache(key, syncedVersion, int64(len(dsApp.Targets)))
		klog.Infof("application %s/%s revision %s synced successful", targetApp.Name, targetApp.Namespace, syncedVersion)
	}

	recordName := oam.GetPublishVersion(targetApp)
	if recordName == "" {
		if targetApp.Status.Workflow != nil {
			recordName = strings.Replace(targetApp.Status.Workflow.AppRevision, ":", "-", 1)
		} else {
			klog.Warningf("app %s/%s has no publish version or revision in status, skip sync workflow status", targetApp.Namespace, targetApp.Name)
		}
	}
	return c.workflowService.SyncWorkflowRecord(ctx, c.getAppMetaName(ctx, targetApp.Name, targetApp.Namespace), recordName, targetApp, nil)
}

// AddOrUpdate will sync application CR to storage of VelaUX automatically
func (c *CR2UX) AddOrUpdate(ctx context.Context, targetApp *v1beta1.Application) error {
	switch {
	case c.appFromUX(targetApp):
		return c.syncAppCreatedByUX(ctx, targetApp)
	case c.appFromCLI(targetApp):
		return c.syncAppCreatedByCLI(ctx, targetApp)
	default:
		klog.Infof("skip syncing application %s/%s", targetApp.Name, targetApp.Namespace)
	}
	return nil
}

// DeleteApp will delete the application as the CR was deleted
func (c *CR2UX) DeleteApp(ctx context.Context, targetApp *v1beta1.Application) error {
	if !c.appFromCLI(targetApp) && !c.shouldSyncMetaFromCLI(ctx, targetApp, true) {
		return nil
	}
	app, appName, err := c.getApp(ctx, targetApp.Name, targetApp.Namespace)
	if err != nil {
		return err
	}
	// Only for the unit test scenario
	if c.applicationService == nil {
		return c.ds.Delete(ctx, &model.Application{Name: appName})
	}
	return c.applicationService.DeleteApplication(ctx, app)
}
