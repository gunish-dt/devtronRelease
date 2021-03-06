package appStoreDeploymentGitopsTool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/argoproj/argo-cd/pkg/apiclient/application"
	client "github.com/devtron-labs/devtron/api/helm-app"
	openapi "github.com/devtron-labs/devtron/api/helm-app/openapiClient"
	application2 "github.com/devtron-labs/devtron/client/argocdServer/application"
	"github.com/devtron-labs/devtron/internal/constants"
	"github.com/devtron-labs/devtron/internal/util"
	appStoreBean "github.com/devtron-labs/devtron/pkg/appStore/bean"
	appStoreDeploymentFullMode "github.com/devtron-labs/devtron/pkg/appStore/deployment/fullMode"
	"github.com/devtron-labs/devtron/pkg/appStore/deployment/repository"
	"github.com/go-pg/pg"
	"github.com/golang/protobuf/ptypes/timestamp"
	"go.uber.org/zap"
	"net/http"
	"strings"
	"time"
)

type AppStoreDeploymentArgoCdService interface {
	InstallApp(installAppVersionRequest *appStoreBean.InstallAppVersionDTO, ctx context.Context) (*appStoreBean.InstallAppVersionDTO, error)
	GetAppStatus(installedAppAndEnvDetails repository.InstalledAppAndEnvDetails, w http.ResponseWriter, r *http.Request, token string) (string, error)
	DeleteInstalledApp(ctx context.Context, appName string, environmentName string, installAppVersionRequest *appStoreBean.InstallAppVersionDTO, installedApps *repository.InstalledApps, dbTransaction *pg.Tx) error
	RollbackRelease(ctx context.Context, installedApp *appStoreBean.InstallAppVersionDTO, deploymentVersion int32) (*appStoreBean.InstallAppVersionDTO, bool, error)
	GetDeploymentHistory(ctx context.Context, installedApp *appStoreBean.InstallAppVersionDTO) (*client.HelmAppDeploymentHistory, error)
	GetDeploymentHistoryInfo(ctx context.Context, installedApp *appStoreBean.InstallAppVersionDTO, version int32) (*openapi.HelmAppDeploymentManifestDetail, error)
}

type AppStoreDeploymentArgoCdServiceImpl struct {
	Logger                            *zap.SugaredLogger
	appStoreDeploymentFullModeService appStoreDeploymentFullMode.AppStoreDeploymentFullModeService
	acdClient                         application2.ServiceClient
	chartGroupDeploymentRepository    repository.ChartGroupDeploymentRepository
	installedAppRepository            repository.InstalledAppRepository
	installedAppRepositoryHistory     repository.InstalledAppVersionHistoryRepository
}

func NewAppStoreDeploymentArgoCdServiceImpl(logger *zap.SugaredLogger, appStoreDeploymentFullModeService appStoreDeploymentFullMode.AppStoreDeploymentFullModeService,
	acdClient application2.ServiceClient, chartGroupDeploymentRepository repository.ChartGroupDeploymentRepository,
	installedAppRepository repository.InstalledAppRepository, installedAppRepositoryHistory repository.InstalledAppVersionHistoryRepository) *AppStoreDeploymentArgoCdServiceImpl {
	return &AppStoreDeploymentArgoCdServiceImpl{
		Logger:                            logger,
		appStoreDeploymentFullModeService: appStoreDeploymentFullModeService,
		acdClient:                         acdClient,
		chartGroupDeploymentRepository:    chartGroupDeploymentRepository,
		installedAppRepository:            installedAppRepository,
		installedAppRepositoryHistory:     installedAppRepositoryHistory,
	}
}

func (impl AppStoreDeploymentArgoCdServiceImpl) InstallApp(installAppVersionRequest *appStoreBean.InstallAppVersionDTO, ctx context.Context) (*appStoreBean.InstallAppVersionDTO, error) {
	//step 2 git operation pull push
	installAppVersionRequest, chartGitAttr, err := impl.appStoreDeploymentFullModeService.AppStoreDeployOperationGIT(installAppVersionRequest)
	if err != nil {
		impl.Logger.Errorw(" error", "err", err)
		return installAppVersionRequest, err
	}

	//step 3 acd operation register, sync
	installAppVersionRequest, err = impl.appStoreDeploymentFullModeService.AppStoreDeployOperationACD(installAppVersionRequest, chartGitAttr, ctx)
	if err != nil {
		impl.Logger.Errorw(" error", "err", err)
		return installAppVersionRequest, err
	}

	return installAppVersionRequest, nil
}

// TODO: Test ACD to get status
func (impl AppStoreDeploymentArgoCdServiceImpl) GetAppStatus(installedAppAndEnvDetails repository.InstalledAppAndEnvDetails, w http.ResponseWriter, r *http.Request, token string) (string, error) {
	if len(installedAppAndEnvDetails.AppName) > 0 && len(installedAppAndEnvDetails.EnvironmentName) > 0 {
		acdAppName := installedAppAndEnvDetails.AppName + "-" + installedAppAndEnvDetails.EnvironmentName
		query := &application.ResourcesQuery{
			ApplicationName: &acdAppName,
		}
		ctx, cancel := context.WithCancel(r.Context())
		if cn, ok := w.(http.CloseNotifier); ok {
			go func(done <-chan struct{}, closed <-chan bool) {
				select {
				case <-done:
				case <-closed:
					cancel()
				}
			}(ctx.Done(), cn.CloseNotify())
		}
		ctx = context.WithValue(ctx, "token", token)
		defer cancel()
		impl.Logger.Debugf("Getting status for app %s in env %s", installedAppAndEnvDetails.AppName, installedAppAndEnvDetails.EnvironmentName)
		start := time.Now()
		resp, err := impl.acdClient.ResourceTree(ctx, query)
		elapsed := time.Since(start)
		impl.Logger.Debugf("Time elapsed %s in fetching application %s for environment %s", elapsed, installedAppAndEnvDetails.AppName, installedAppAndEnvDetails.EnvironmentName)
		if err != nil {
			impl.Logger.Errorw("error fetching resource tree", "error", err)
			err = &util.ApiError{
				Code:            constants.AppDetailResourceTreeNotFound,
				InternalMessage: "app detail fetched, failed to get resource tree from acd",
				UserMessage:     "app detail fetched, failed to get resource tree from acd",
			}
			return "", err

		}
		return resp.Status, nil
	}
	return "", errors.New("invalid app name or env name")
}

func (impl AppStoreDeploymentArgoCdServiceImpl) DeleteInstalledApp(ctx context.Context, appName string, environmentName string, installAppVersionRequest *appStoreBean.InstallAppVersionDTO, installedApps *repository.InstalledApps, dbTransaction *pg.Tx) error {
	acdAppName := appName + "-" + environmentName
	err := impl.deleteACD(acdAppName, ctx)
	if err != nil {
		impl.Logger.Errorw("error in deleting ACD ", "name", acdAppName, "err", err)
		if installAppVersionRequest.ForceDelete {
			impl.Logger.Warnw("error while deletion of app in acd, continue to delete in db as this operation is force delete", "error", err)
		} else {
			//statusError, _ := err.(*errors2.StatusError)
			if strings.Contains(err.Error(), "code = NotFound") {
				err = &util.ApiError{
					UserMessage:     "Could not delete as application not found in argocd",
					InternalMessage: err.Error(),
				}
			} else {
				err = &util.ApiError{
					UserMessage:     "Could not delete application",
					InternalMessage: err.Error(),
				}
			}
			return err
		}
	}
	deployment, err := impl.chartGroupDeploymentRepository.FindByInstalledAppId(installedApps.Id)
	if err != nil && err != pg.ErrNoRows {
		impl.Logger.Errorw("error in fetching chartGroupMapping", "id", installedApps.Id, "err", err)
		return err
	} else if err == pg.ErrNoRows {
		impl.Logger.Infow("not a chart group deployment skipping chartGroupMapping delete", "id", installedApps.Id)
	} else {
		deployment.Deleted = true
		deployment.UpdatedOn = time.Now()
		deployment.UpdatedBy = installAppVersionRequest.UserId
		_, err := impl.chartGroupDeploymentRepository.Update(deployment, dbTransaction)
		if err != nil {
			impl.Logger.Errorw("error in mapping delete", "err", err)
			return err
		}
	}

	return nil
}

// returns - valuesYamlStr, success, error
func (impl AppStoreDeploymentArgoCdServiceImpl) RollbackRelease(ctx context.Context, installedApp *appStoreBean.InstallAppVersionDTO, installedAppVersionHistoryId int32) (*appStoreBean.InstallAppVersionDTO, bool, error) {
	//request version id for
	versionHistory, err := impl.installedAppRepositoryHistory.GetInstalledAppVersionHistory(int(installedAppVersionHistoryId))
	if err != nil {
		impl.Logger.Errorw("error", "err", err)
		err = &util.ApiError{Code: "404", HttpStatusCode: 404, UserMessage: fmt.Sprintf("No deployment history version found for id: %d", installedAppVersionHistoryId), InternalMessage: err.Error()}
		return installedApp, false, err
	}
	installedAppVersion, err := impl.installedAppRepository.GetInstalledAppVersionAny(versionHistory.InstalledAppVersionId)
	if err != nil {
		impl.Logger.Errorw("error", "err", err)
		err = &util.ApiError{Code: "404", HttpStatusCode: 404, UserMessage: fmt.Sprintf("No installed app version found for id: %d", versionHistory.InstalledAppVersionId), InternalMessage: err.Error()}
		return installedApp, false, err
	}
	activeInstalledAppVersion, err := impl.installedAppRepository.GetActiveInstalledAppVersionByInstalledAppId(installedApp.InstalledAppId)
	if err != nil {
		impl.Logger.Errorw("error", "err", err)
		return installedApp, false, err
	}

	//validate relations
	if installedApp.InstalledAppId != installedAppVersion.InstalledAppId {
		err = &util.ApiError{Code: "400", HttpStatusCode: 400, UserMessage: "bad request, requested version are not belongs to each other", InternalMessage: ""}
		return installedApp, false, err
	}

	installedApp.InstalledAppVersionId = installedAppVersion.Id
	installedApp.AppStoreVersion = installedAppVersion.AppStoreApplicationVersionId
	installedApp.ValuesOverrideYaml = versionHistory.ValuesYamlRaw
	installedApp.AppStoreId = installedAppVersion.AppStoreApplicationVersion.AppStoreId
	installedApp.AppStoreName = installedAppVersion.AppStoreApplicationVersion.AppStore.Name
	installedApp.GitOpsRepoName = installedAppVersion.InstalledApp.GitOpsRepoName
	installedApp.ACDAppName = fmt.Sprintf("%s-%s", installedApp.AppName, installedApp.EnvironmentName)
	//If current version upgrade/degrade to another, update requirement dependencies
	if versionHistory.InstalledAppVersionId != activeInstalledAppVersion.Id {
		err = impl.appStoreDeploymentFullModeService.UpdateRequirementYaml(installedApp, &installedAppVersion.AppStoreApplicationVersion)
		if err != nil {
			impl.Logger.Errorw("error", "err", err)
			return installedApp, false, nil
		}

		activeInstalledAppVersion.Active = false
		_, err = impl.installedAppRepository.UpdateInstalledAppVersion(activeInstalledAppVersion, nil)
		if err != nil {
			impl.Logger.Errorw("error", "err", err)
			return installedApp, false, nil
		}
	}
	//Update Values config
	installedApp, err = impl.appStoreDeploymentFullModeService.UpdateValuesYaml(installedApp)
	if err != nil {
		impl.Logger.Errorw("error", "err", err)
		return installedApp, false, nil
	}
	//ACD sync operation
	impl.appStoreDeploymentFullModeService.SyncACD(installedApp.ACDAppName, ctx)
	return installedApp, true, nil
}

func (impl AppStoreDeploymentArgoCdServiceImpl) deleteACD(acdAppName string, ctx context.Context) error {
	req := new(application.ApplicationDeleteRequest)
	req.Name = &acdAppName
	if ctx == nil {
		impl.Logger.Errorw("err in delete ACD for AppStore, ctx is NULL", "acdAppName", acdAppName)
		return fmt.Errorf("context is null")
	}
	if _, err := impl.acdClient.Delete(ctx, req); err != nil {
		impl.Logger.Errorw("err in delete ACD for AppStore", "acdAppName", acdAppName, "err", err)
		return err
	}
	return nil
}
func (impl AppStoreDeploymentArgoCdServiceImpl) getSourcesFromManifest(chartYaml string) ([]string, error) {
	var b map[string]interface{}
	var sources []string
	err := json.Unmarshal([]byte(chartYaml), &b)
	if err != nil {
		impl.Logger.Errorw("error while unmarshal chart yaml", "error", err)
		return sources, err
	}
	if b != nil && b["sources"] != nil {
		slice := b["sources"].([]interface{})
		for _, item := range slice {
			sources = append(sources, item.(string))
		}
	}
	return sources, nil
}
func (impl AppStoreDeploymentArgoCdServiceImpl) GetDeploymentHistory(ctx context.Context, installedAppDto *appStoreBean.InstallAppVersionDTO) (*client.HelmAppDeploymentHistory, error) {
	result := &client.HelmAppDeploymentHistory{}
	var history []*client.HelmAppDeploymentDetail
	//TODO - response setup

	installedAppVersions, err := impl.installedAppRepository.GetInstalledAppVersionByInstalledAppIdMeta(installedAppDto.InstalledAppId)
	if err != nil {
		impl.Logger.Errorw("error while fetching installed version", "error", err)
		return result, err
	}
	for _, installedAppVersionModel := range installedAppVersions {

		sources, err := impl.getSourcesFromManifest(installedAppVersionModel.AppStoreApplicationVersion.ChartYaml)
		if err != nil {
			impl.Logger.Errorw("error while fetching sources", "error", err)
			//continues here, skip error in case found issue on fetching source
		}
		versionHistory, err := impl.installedAppRepositoryHistory.GetInstalledAppVersionHistoryByVersionId(installedAppVersionModel.Id)
		if err != nil && err != pg.ErrNoRows {
			impl.Logger.Errorw("error while fetching installed version history", "error", err)
			return result, err
		}
		for _, updateHistory := range versionHistory {
			history = append(history, &client.HelmAppDeploymentDetail{
				ChartMetadata: &client.ChartMetadata{
					ChartName:    installedAppVersionModel.AppStoreApplicationVersion.AppStore.Name,
					ChartVersion: installedAppVersionModel.AppStoreApplicationVersion.Version,
					Description:  installedAppVersionModel.AppStoreApplicationVersion.Description,
					Home:         installedAppVersionModel.AppStoreApplicationVersion.Home,
					Sources:      sources,
				},
				DockerImages: []string{installedAppVersionModel.AppStoreApplicationVersion.AppVersion},
				DeployedAt: &timestamp.Timestamp{
					Seconds: updateHistory.UpdatedOn.Unix(),
					Nanos:   int32(updateHistory.UpdatedOn.Nanosecond()),
				},
				Version: int32(updateHistory.Id),
			})
		}
	}

	if len(history) == 0 {
		history = make([]*client.HelmAppDeploymentDetail, 0)
	}
	result.DeploymentHistory = history
	return result, err
}

func (impl AppStoreDeploymentArgoCdServiceImpl) GetDeploymentHistoryInfo(ctx context.Context, installedApp *appStoreBean.InstallAppVersionDTO, version int32) (*openapi.HelmAppDeploymentManifestDetail, error) {
	values := &openapi.HelmAppDeploymentManifestDetail{}
	versionHistory, err := impl.installedAppRepositoryHistory.GetInstalledAppVersionHistory(int(version))
	if err != nil {
		impl.Logger.Errorw("error while fetching installed version history", "error", err)
		return nil, err
	}
	values.ValuesYaml = &versionHistory.ValuesYamlRaw
	return values, err
}
