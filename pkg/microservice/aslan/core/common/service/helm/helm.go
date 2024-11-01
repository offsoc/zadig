/*
Copyright 2021 The KodeRover Authors.

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

package helm

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"time"

	"go.uber.org/zap"

	"github.com/koderover/zadig/v2/pkg/microservice/aslan/config"
	"github.com/koderover/zadig/v2/pkg/microservice/aslan/core/common/repository/models"
	commonmodels "github.com/koderover/zadig/v2/pkg/microservice/aslan/core/common/repository/models"
	commonrepo "github.com/koderover/zadig/v2/pkg/microservice/aslan/core/common/repository/mongodb"
	"github.com/koderover/zadig/v2/pkg/microservice/aslan/core/common/repository/mongodb/template"
	fsservice "github.com/koderover/zadig/v2/pkg/microservice/aslan/core/common/service/fs"
	commonutil "github.com/koderover/zadig/v2/pkg/microservice/aslan/core/common/util"
	"github.com/koderover/zadig/v2/pkg/setting"
	"github.com/koderover/zadig/v2/pkg/tool/cache"
	"github.com/koderover/zadig/v2/pkg/tool/crypto"
	"github.com/koderover/zadig/v2/pkg/tool/log"
	"github.com/koderover/zadig/v2/pkg/tool/mongo"
	"github.com/pkg/errors"
)

const (
	UpdateHelmEnvLockKey = "UpdateHelmEnv"
)

func ListHelmRepos(encryptedKey string, log *zap.SugaredLogger) ([]*commonmodels.HelmRepo, error) {
	aesKey, err := commonutil.GetAesKeyFromEncryptedKey(encryptedKey, log)
	if err != nil {
		log.Errorf("ListHelmRepos GetAesKeyFromEncryptedKey err:%v", err)
		return nil, err
	}
	helmRepos, err := commonrepo.NewHelmRepoColl().List()
	if err != nil {
		log.Errorf("ListHelmRepos err:%v", err)
		return []*commonmodels.HelmRepo{}, nil
	}
	for _, helmRepo := range helmRepos {
		helmRepo.Password, err = crypto.AesEncryptByKey(helmRepo.Password, aesKey.PlainText)
		if err != nil {
			log.Errorf("ListHelmRepos AesEncryptByKey err:%v", err)
			return nil, err
		}
	}
	return helmRepos, nil
}

func ListHelmReposByProject(projectName string, log *zap.SugaredLogger) ([]*commonmodels.HelmRepo, error) {
	helmRepos, err := commonrepo.NewHelmRepoColl().ListByProject(projectName)
	if err != nil {
		log.Errorf("ListHelmRepos err:%v", err)
		return []*commonmodels.HelmRepo{}, nil
	}
	for _, helmRepo := range helmRepos {
		helmRepo.Password = ""
		helmRepo.Projects = nil
	}
	return helmRepos, nil
}

func ListHelmReposPublic() ([]*commonmodels.HelmRepo, error) {
	return commonrepo.NewHelmRepoColl().List()
}

func SaveAndUploadService(projectName, serviceName string, copies []string, fileTree fs.FS, isProduction bool) error {
	var localBase, s3Base string
	if !isProduction {
		localBase = config.LocalTestServicePath(projectName, serviceName)
		s3Base = config.ObjectStorageTestServicePath(projectName, serviceName)
	} else {
		localBase = config.LocalProductionServicePath(projectName, serviceName)
		s3Base = config.ObjectStorageProductionServicePath(projectName, serviceName)
	}
	names := append([]string{serviceName}, copies...)
	return fsservice.SaveAndUploadFiles(fileTree, names, localBase, s3Base, log.SugaredLogger())
}

func CopyAndUploadService(projectName, serviceName, currentChartPath string, copies []string, isProduction bool) error {
	var localBase, s3Base string
	if !isProduction {
		localBase = config.LocalTestServicePath(projectName, serviceName)
		s3Base = config.ObjectStorageTestServicePath(projectName, serviceName)
	} else {
		localBase = config.LocalProductionServicePath(projectName, serviceName)
		s3Base = config.ObjectStorageProductionServicePath(projectName, serviceName)
	}
	names := append([]string{serviceName}, copies...)

	return fsservice.CopyAndUploadFiles(names, path.Join(localBase, serviceName), s3Base, localBase, currentChartPath, log.SugaredLogger())
}

// Update helm Service and ServiceDeployStrategy for a single service in environment
func UpdateHelmServiceInEnv(product *commonmodels.Product, productSvc *commonmodels.ProductService, user string) error {
	session := mongo.Session()
	defer session.EndSession(context.TODO())

	err := mongo.StartTransaction(session)
	if err != nil {
		return err
	}

	product.LintServices()
	err = commonutil.CreateEnvServiceVersion(product, productSvc, user, session, log.SugaredLogger())
	if err != nil {
		log.Errorf("failed to create helm service version, err: %v", err)
	}

	envLock := cache.NewRedisLock(fmt.Sprintf("%s:%s:%s", UpdateHelmEnvLockKey, product.ProductName, product.EnvName))
	envLock.Lock()
	defer envLock.Unlock()

	productColl := commonrepo.NewProductCollWithSession(session)
	newProductInfo, err := productColl.Find(&commonrepo.ProductFindOptions{Name: product.ProductName, EnvName: product.EnvName})
	if err != nil {
		mongo.AbortTransaction(session)
		return errors.Wrapf(err, "failed to find product %s", product.ProductName)
	}

	newProductInfo.LintServices()
	productSvcMap := newProductInfo.GetServiceMap()
	productChartSvcMap := newProductInfo.GetChartServiceMap()
	if productSvc.FromZadig() {
		productSvcMap[productSvc.ServiceName] = productSvc
		productSvcMap[productSvc.ServiceName].UpdateTime = time.Now().Unix()
		delete(productChartSvcMap, productSvc.ReleaseName)
	} else {
		productChartSvcMap[productSvc.ReleaseName] = productSvc
		productChartSvcMap[productSvc.ReleaseName].UpdateTime = time.Now().Unix()
		for _, svc := range productSvcMap {
			if svc.ReleaseName == productSvc.ReleaseName {
				delete(productSvcMap, svc.ServiceName)
				break
			}
		}
	}

	templateProduct, err := template.NewProductCollWithSess(session).Find(product.ProductName)
	if err != nil {
		mongo.AbortTransaction(session)
		return errors.Wrapf(err, "failed to find template product %s", product.ProductName)
	}

	newProductInfo.Services = [][]*commonmodels.ProductService{}
	serviceOrchestration := templateProduct.Services
	if product.Production {
		serviceOrchestration = templateProduct.ProductionServices
	}

	for i, svcGroup := range serviceOrchestration {
		// init slice
		if len(newProductInfo.Services) >= i {
			newProductInfo.Services = append(newProductInfo.Services, []*commonmodels.ProductService{})
		}

		// set services in order
		for _, svc := range svcGroup {
			// if svc exists in productSvcMap
			if productSvcMap[svc] != nil {
				newProductInfo.Services[i] = append(newProductInfo.Services[i], productSvcMap[svc])
			}
		}
	}
	// append chart services to the last group
	for _, service := range productChartSvcMap {
		newProductInfo.Services[len(newProductInfo.Services)-1] = append(newProductInfo.Services[len(newProductInfo.Services)-1], service)
	}

	if productSvc.DeployStrategy == setting.ServiceDeployStrategyDeploy {
		if productSvc.FromZadig() {
			newProductInfo.ServiceDeployStrategy = commonutil.SetServiceDeployStrategyDepoly(newProductInfo.ServiceDeployStrategy, productSvc.ServiceName)
		} else {
			newProductInfo.ServiceDeployStrategy = commonutil.SetChartServiceDeployStrategyDepoly(newProductInfo.ServiceDeployStrategy, productSvc.ReleaseName)
		}
	} else if productSvc.DeployStrategy == setting.ServiceDeployStrategyImport {
		if productSvc.FromZadig() {
			newProductInfo.ServiceDeployStrategy = commonutil.SetServiceDeployStrategyImport(newProductInfo.ServiceDeployStrategy, productSvc.ServiceName)
		} else {
			newProductInfo.ServiceDeployStrategy = commonutil.SetChartServiceDeployStrategyImport(newProductInfo.ServiceDeployStrategy, productSvc.ReleaseName)
		}
	}

	if err = productColl.Update(newProductInfo); err != nil {
		log.Errorf("update product %s error: %s", newProductInfo.ProductName, err.Error())
		mongo.AbortTransaction(session)
		return fmt.Errorf("failed to update product info, name %s", newProductInfo.ProductName)
	}

	return mongo.CommitTransaction(session)
}

// Update all helm services in environment
func UpdateHelmAllServicesInEnv(productName, envName string, services [][]*models.ProductService, production bool) error {
	session := mongo.Session()
	defer session.EndSession(context.TODO())

	err := mongo.StartTransaction(session)
	if err != nil {
		return err
	}

	productColl := commonrepo.NewProductCollWithSession(session)

	envLock := cache.NewRedisLock(fmt.Sprintf("%s:%s:%s", UpdateHelmEnvLockKey, productName, envName))
	envLock.Lock()
	defer envLock.Unlock()

	templateProduct, err := template.NewProductCollWithSess(session).Find(productName)
	if err != nil {
		mongo.AbortTransaction(session)
		return errors.Wrapf(err, "failed to find template product %s", productName)
	}

	serviceOrchestration := templateProduct.Services
	if production {
		serviceOrchestration = templateProduct.ProductionServices
	}

	dummyEnv := &commonmodels.Product{
		Services: services,
	}
	dummyEnv.LintServices()
	productSvcMap := dummyEnv.GetServiceMap()
	productChartSvcMap := dummyEnv.GetChartServiceMap()

	newServices := [][]*commonmodels.ProductService{}
	for i, svcGroup := range serviceOrchestration {
		// init slice
		if len(newServices) >= i {
			newServices = append(newServices, []*commonmodels.ProductService{})
		}

		// set services in order
		for _, svc := range svcGroup {
			// if svc exists in productSvcMap
			if productSvcMap[svc] != nil {
				newServices[i] = append(newServices[i], productSvcMap[svc])
			}
		}
	}
	// append chart services to the last group
	for _, service := range productChartSvcMap {
		newServices[len(newServices)-1] = append(newServices[len(newServices)-1], service)
	}

	if err = productColl.UpdateAllServices(productName, envName, newServices); err != nil {
		err = fmt.Errorf("failed to update %s/%s product services, err %s", productName, envName, err)
		mongo.AbortTransaction(session)
		log.Error(err)
		return err
	}

	return mongo.CommitTransaction(session)
}

// Update a helm services group in environment
func UpdateHelmServicesGroupInEnv(productName, envName string, index int, group []*models.ProductService, production bool) error {
	session := mongo.Session()
	defer session.EndSession(context.TODO())

	err := mongo.StartTransaction(session)
	if err != nil {
		return err
	}

	productColl := commonrepo.NewProductCollWithSession(session)

	envLock := cache.NewRedisLock(fmt.Sprintf("%s:%s:%s", UpdateHelmEnvLockKey, productName, envName))
	envLock.Lock()
	defer envLock.Unlock()

	templateProduct, err := template.NewProductCollWithSess(session).Find(productName)
	if err != nil {
		mongo.AbortTransaction(session)
		return errors.Wrapf(err, "failed to find template product %s", productName)
	}

	serviceOrchestration := templateProduct.Services
	if production {
		serviceOrchestration = templateProduct.ProductionServices
	}

	dummyEnv := &commonmodels.Product{
		Services: [][]*commonmodels.ProductService{group},
	}
	dummyEnv.LintServices()
	productSvcMap := dummyEnv.GetServiceMap()
	productChartSvcMap := dummyEnv.GetChartServiceMap()

	newGroup := []*commonmodels.ProductService{}
	for _, svcGroup := range serviceOrchestration {
		// set services in order
		for _, svc := range svcGroup {
			// if svc exists in productSvcMap
			if productSvcMap[svc] != nil {
				newGroup = append(newGroup, productSvcMap[svc])
			}
		}
	}
	// append chart services to the last group
	for _, service := range productChartSvcMap {
		newGroup = append(newGroup, service)
	}

	if err = productColl.UpdateServicesGroup(productName, envName, index, newGroup); err != nil {
		err = fmt.Errorf("failed to update %s/%s product services, err %s", productName, envName, err)
		mongo.AbortTransaction(session)
		log.Error(err)
		return err
	}

	return mongo.CommitTransaction(session)
}