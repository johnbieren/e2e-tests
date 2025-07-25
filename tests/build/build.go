package build

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/devfile/library/v2/pkg/util"
	"github.com/google/go-github/v44/github"
	appservice "github.com/konflux-ci/application-api/api/v1alpha1"
	"github.com/konflux-ci/build-service/controllers"
	tektonutils "github.com/konflux-ci/release-service/tekton/utils"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/openshift/library-go/pkg/image/reference"
	pipeline "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	v1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/konflux-ci/e2e-tests/pkg/clients/git"
	"github.com/konflux-ci/e2e-tests/pkg/clients/has"
	"github.com/konflux-ci/e2e-tests/pkg/constants"
	"github.com/konflux-ci/e2e-tests/pkg/framework"
	"github.com/konflux-ci/e2e-tests/pkg/utils"
	"github.com/konflux-ci/e2e-tests/pkg/utils/build"
)

var _ = framework.BuildSuiteDescribe("Build service E2E tests", Label("build-service"), func() {

	var f *framework.Framework
	AfterEach(framework.ReportFailure(&f))
	var err error
	defer GinkgoRecover()

	var gitClient git.Client

	DescribeTableSubtree("test PaC component build", Ordered, Label("github-webhook", "pac-build", "pipeline", "image-controller"), func(gitProvider git.GitProvider, gitPrefix string) {
		var applicationName, customDefaultComponentName, customBranchComponentName, componentBaseBranchName string
		var pacBranchName, testNamespace, imageRepoName, pullRobotAccountName, pushRobotAccountName string
		var helloWorldComponentGitSourceURL, customDefaultComponentBranch string
		var component *appservice.Component
		var plr *pipeline.PipelineRun

		var timeout, interval time.Duration

		var prNumber int
		var prHeadSha string
		var buildPipelineAnnotation map[string]string

		var helloWorldRepository string

		BeforeAll(func() {
			if os.Getenv(constants.SKIP_PAC_TESTS_ENV) == "true" {
				Skip("Skipping this test due to configuration issue with Spray proxy")
			}

			f, err = framework.NewFramework(utils.GetGeneratedNamespace("build-e2e"))
			Expect(err).NotTo(HaveOccurred())
			testNamespace = f.UserNamespace

			if utils.IsPrivateHostname(f.OpenshiftConsoleHost) {
				Skip("Using private cluster (not reachable from Github), skipping...")
			}

			quayOrg := utils.GetEnv("DEFAULT_QUAY_ORG", "")
			supports, err := build.DoesQuayOrgSupportPrivateRepo()
			Expect(err).ShouldNot(HaveOccurred(), fmt.Sprintf("error while checking if quay org supports private repo: %+v", err))
			if !supports {
				if quayOrg == "redhat-appstudio-qe" {
					Fail("Failed to create private image repo in redhat-appstudio-qe org")
				} else {
					Skip("Quay org does not support private quay repository creation, please add support for private repo creation before running this test")
				}
			}
			Expect(err).ShouldNot(HaveOccurred())

			applicationName = fmt.Sprintf("build-suite-test-application-%s", util.GenerateRandomString(4))
			_, err = f.AsKubeAdmin.HasController.CreateApplication(applicationName, testNamespace)
			Expect(err).NotTo(HaveOccurred())

			customDefaultComponentName = fmt.Sprintf("%s-%s-%s", gitPrefix, "test-custom-default", util.GenerateRandomString(6))
			customBranchComponentName = fmt.Sprintf("%s-%s-%s", gitPrefix, "test-custom-branch", util.GenerateRandomString(6))
			pacBranchName = constants.PaCPullRequestBranchPrefix + customBranchComponentName
			customDefaultComponentBranch = constants.PaCPullRequestBranchPrefix + customDefaultComponentName
			componentBaseBranchName = fmt.Sprintf("base-%s", util.GenerateRandomString(6))

			gitClient, helloWorldComponentGitSourceURL, helloWorldRepository = setupGitProvider(f, gitProvider)
			// get the build pipeline bundle annotation
			buildPipelineAnnotation = build.GetBuildPipelineBundleAnnotation(constants.DockerBuild)

			err = gitClient.CreateBranch(helloWorldRepository, helloWorldComponentDefaultBranch, helloWorldComponentRevision, componentBaseBranchName)
			Expect(err).ShouldNot(HaveOccurred())
		})

		AfterAll(func() {
			if !CurrentSpecReport().Failed() {
				Expect(f.AsKubeAdmin.HasController.DeleteAllComponentsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
				Expect(f.AsKubeAdmin.HasController.DeleteAllApplicationsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
			}

			err = gitClient.DeleteBranch(helloWorldRepository, pacBranchName)
			if err != nil {
				Expect(err.Error()).To(Or(ContainSubstring("Reference does not exist"), ContainSubstring("404")))
			}
			err = gitClient.DeleteBranch(helloWorldRepository, componentBaseBranchName)
			if err != nil {
				Expect(err.Error()).To(Or(ContainSubstring("Reference does not exist"), ContainSubstring("404")))
			}

			err := gitClient.DeleteBranchAndClosePullRequest(helloWorldRepository, prNumber)
			if err != nil {
				Expect(err.Error()).To(Or(ContainSubstring("Reference does not exist"), ContainSubstring("404")))
			}

			Expect(gitClient.CleanupWebhooks(helloWorldRepository, f.ClusterAppDomain)).To(Succeed())
		})

		When("a new component without specified branch is created and with visibility private", Label("pac-custom-default-branch"), func() {
			var componentObj appservice.ComponentSpec

			BeforeAll(func() {
				componentObj = appservice.ComponentSpec{
					ComponentName: customDefaultComponentName,
					Application:   applicationName,
					Source: appservice.ComponentSource{
						ComponentSourceUnion: appservice.ComponentSourceUnion{
							GitSource: &appservice.GitSource{
								URL:           helloWorldComponentGitSourceURL,
								Revision:      "",
								DockerfileURL: constants.DockerFilePath,
							},
						},
					},
				}

				_, err = f.AsKubeAdmin.HasController.CreateComponent(componentObj, testNamespace, "", "", applicationName, false, utils.MergeMaps(utils.MergeMaps(constants.ComponentPaCRequestAnnotation, constants.ImageControllerAnnotationRequestPrivateRepo), buildPipelineAnnotation))
				Expect(err).ShouldNot(HaveOccurred())
			})

			It("correctly targets the default branch (that is not named 'main') with PaC", func() {
				timeout = time.Second * 300
				interval = time.Second * 1
				Eventually(func() bool {
					prs, err := gitClient.ListPullRequests(helloWorldRepository)
					Expect(err).ShouldNot(HaveOccurred())

					for _, pr := range prs {
						if pr.SourceBranch == customDefaultComponentBranch {
							Expect(pr.TargetBranch).To(Equal(helloWorldComponentDefaultBranch))
							return true
						}
					}
					return false
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("timed out when waiting for init PaC PR to be created against %s branch in %s repository", helloWorldComponentDefaultBranch, helloWorldComponentGitSourceRepoName))
			})

			It("workspace parameter is set correctly in PaC repository CR", func() {
				nsObj, err := f.AsKubeAdmin.CommonController.GetNamespace(testNamespace)
				Expect(err).ShouldNot(HaveOccurred())
				wsName := nsObj.Labels["appstudio.redhat.com/workspace_name"]
				repositoryParams, err := f.AsKubeAdmin.TektonController.GetRepositoryParams(customDefaultComponentName, testNamespace)
				Expect(err).ShouldNot(HaveOccurred(), "error while trying to get repository params")
				paramExists := false
				for _, param := range repositoryParams {
					if param.Name == "appstudio_workspace" {
						paramExists = true
						Expect(param.Value).To(Equal(wsName), fmt.Sprintf("got workspace param value: %s, expected %s", param.Value, wsName))
					}
				}
				Expect(paramExists).To(BeTrue(), "appstudio_workspace param does not exists in repository CR")

			})
			It("triggers a PipelineRun", func() {
				timeout = time.Minute * 5
				Eventually(func() error {
					plr, err = f.AsKubeAdmin.HasController.GetComponentPipelineRun(customDefaultComponentName, applicationName, testNamespace, "")
					if err != nil {
						GinkgoWriter.Printf("PipelineRun has not been created yet for the component %s/%s\n", testNamespace, customBranchComponentName)
						return err
					}
					if !plr.HasStarted() {
						return fmt.Errorf("pipelinerun %s/%s hasn't started yet", plr.GetNamespace(), plr.GetName())
					}
					return nil
				}, timeout, constants.PipelineRunPollingInterval).Should(Succeed(), fmt.Sprintf("timed out when waiting for the PipelineRun to start for the component %s/%s", customBranchComponentName, testNamespace))
			})
			It("build pipeline uses the correct serviceAccount", func() {
				serviceAccountName := "build-pipeline-" + customDefaultComponentName
				Expect(plr.Spec.TaskRunTemplate.ServiceAccountName).Should(Equal(serviceAccountName))
			})
			It("component build status is set correctly", func() {
				var buildStatus *controllers.BuildStatus
				Eventually(func() (bool, error) {
					component, err := f.AsKubeAdmin.HasController.GetComponent(customDefaultComponentName, testNamespace)
					if err != nil {
						return false, err
					}

					buildStatusAnnotationValue := component.Annotations[controllers.BuildStatusAnnotationName]
					GinkgoWriter.Printf(buildStatusAnnotationValueLoggingFormat, buildStatusAnnotationValue)
					statusBytes := []byte(buildStatusAnnotationValue)

					err = json.Unmarshal(statusBytes, &buildStatus)
					if err != nil {
						return false, err
					}

					if buildStatus.PaC != nil {
						GinkgoWriter.Printf("state: %s\n", buildStatus.PaC.State)
						GinkgoWriter.Printf("mergeUrl: %s\n", buildStatus.PaC.MergeUrl)
						GinkgoWriter.Printf("errId: %d\n", buildStatus.PaC.ErrId)
						GinkgoWriter.Printf("errMessage: %s\n", buildStatus.PaC.ErrMessage)
						GinkgoWriter.Printf("configurationTime: %s\n", buildStatus.PaC.ConfigurationTime)
					} else {
						GinkgoWriter.Println("build status does not have PaC field")
					}

					return buildStatus.PaC != nil && buildStatus.PaC.State == "enabled" && buildStatus.PaC.MergeUrl != "" && buildStatus.PaC.ErrId == 0 && buildStatus.PaC.ConfigurationTime != "", nil
				}, timeout, interval).Should(BeTrue(), "component build status has unexpected content")
			})
			It("image repo and robot account created successfully", func() {
				imageRepoName, err = f.AsKubeAdmin.ImageController.GetImageName(testNamespace, customDefaultComponentName)
				Expect(err).ShouldNot(HaveOccurred(), "failed to read image repo for component %s", customDefaultComponentName)
				Expect(imageRepoName).ShouldNot(BeEmpty(), "image repo name is empty")

				imageExist, err := build.DoesImageRepoExistInQuay(imageRepoName)
				Expect(err).ShouldNot(HaveOccurred(), "failed while checking if image repo exists in quay with error: %+v", err)
				Expect(imageExist).To(BeTrue(), "quay image does not exists")

				pullRobotAccountName, pushRobotAccountName, err = f.AsKubeAdmin.ImageController.GetRobotAccounts(testNamespace, customDefaultComponentName)
				Expect(err).ShouldNot(HaveOccurred(), "failed to get robot account names")
				pullRobotAccountExist, err := build.DoesRobotAccountExistInQuay(pullRobotAccountName)
				Expect(err).ShouldNot(HaveOccurred(), "failed while checking if pull robot account exists in quay with error: %+v", err)
				Expect(pullRobotAccountExist).To(BeTrue(), "pull robot account does not exists in quay")
				pushRobotAccountExist, err := build.DoesRobotAccountExistInQuay(pushRobotAccountName)
				Expect(err).ShouldNot(HaveOccurred(), "failed while checking if push robot account exists in quay with error: %+v", err)
				Expect(pushRobotAccountExist).To(BeTrue(), "push robot account does not exists in quay")
			})
			It("created image repo is private", func() {
				isPublic, err := build.IsImageRepoPublic(imageRepoName)
				Expect(err).ShouldNot(HaveOccurred(), fmt.Sprintf("failed while checking if the image repo %s is private", imageRepoName))
				Expect(isPublic).To(BeFalse(), "Expected image repo to be private, but it is public")
			})

			It("a related PipelineRun should be deleted after deleting the component", func() {
				timeout = time.Second * 180
				interval = time.Second * 5
				Expect(f.AsKubeAdmin.HasController.DeleteComponent(customDefaultComponentName, testNamespace, true)).To(Succeed())
				// Test removal of PipelineRun
				Eventually(func() error {
					plr, err = f.AsKubeAdmin.HasController.GetComponentPipelineRun(customDefaultComponentName, applicationName, testNamespace, "")
					if err == nil {
						return fmt.Errorf("pipelinerun %s/%s is not removed yet", plr.GetNamespace(), plr.GetName())
					}
					return err
				}, timeout, interval).Should(MatchError(ContainSubstring("no pipelinerun found")), fmt.Sprintf("timed out when waiting for the PipelineRun to be removed for Component %s/%s", testNamespace, customBranchComponentName))
			})

			It("PR branch should not exist in the repo", func() {
				timeout = time.Second * 60
				interval = time.Second * 1
				Eventually(func() bool {
					exists, err := gitClient.BranchExists(helloWorldRepository, customDefaultComponentBranch)
					Expect(err).ShouldNot(HaveOccurred())
					return exists
				}, timeout, interval).Should(BeFalse(), fmt.Sprintf("timed out when waiting for the branch %s to be deleted from %s repository", customDefaultComponentBranch, helloWorldComponentGitSourceRepoName))
			})

			It("related image repo and the robot account should be deleted after deleting the component", func() {
				timeout = time.Second * 60
				interval = time.Second * 1
				// Check image repo should be deleted
				Eventually(func() (bool, error) {
					return build.DoesImageRepoExistInQuay(imageRepoName)
				}, timeout, interval).Should(BeFalse(), fmt.Sprintf("timed out when waiting for image repo %s to be deleted", imageRepoName))

				// Check robot account should be deleted
				Eventually(func() (bool, error) {
					pullRobotAccountExists, err := build.DoesRobotAccountExistInQuay(pullRobotAccountName)
					if err != nil {
						return false, err
					}
					pushRobotAccountExists, err := build.DoesRobotAccountExistInQuay(pushRobotAccountName)
					if err != nil {
						return false, err
					}
					return pullRobotAccountExists || pushRobotAccountExists, nil
				}, timeout, interval).Should(BeFalse(), fmt.Sprintf("timed out when checking if robot accounts %s and %s got deleted", pullRobotAccountName, pushRobotAccountName))

			})
		})

		When("a new Component with specified custom branch is created", Label("build-custom-branch"), func() {
			var outputImage string
			var componentObj appservice.ComponentSpec

			BeforeAll(func() {
				componentObj = appservice.ComponentSpec{
					ComponentName: customBranchComponentName,
					Application:   applicationName,
					Source: appservice.ComponentSource{
						ComponentSourceUnion: appservice.ComponentSourceUnion{
							GitSource: &appservice.GitSource{
								URL:           helloWorldComponentGitSourceURL,
								Revision:      componentBaseBranchName,
								DockerfileURL: constants.DockerFilePath,
							},
						},
					},
				}
				// Create a component with Git Source URL, a specified git branch and marking delete-repo=true
				component, err = f.AsKubeAdmin.HasController.CreateComponent(componentObj, testNamespace, "", "", applicationName, false, utils.MergeMaps(utils.MergeMaps(constants.ComponentPaCRequestAnnotation, constants.ImageControllerAnnotationRequestPublicRepo), buildPipelineAnnotation))
				Expect(err).ShouldNot(HaveOccurred())
			})

			It("triggers a PipelineRun", func() {
				timeout = time.Second * 600
				interval = time.Second * 1
				Eventually(func() error {
					plr, err = f.AsKubeAdmin.HasController.GetComponentPipelineRun(customBranchComponentName, applicationName, testNamespace, "")
					if err != nil {
						GinkgoWriter.Printf("PipelineRun has not been created yet for the component %s/%s\n", testNamespace, customBranchComponentName)
						return err
					}
					if !plr.HasStarted() {
						return fmt.Errorf("pipelinerun %s/%s hasn't started yet", plr.GetNamespace(), plr.GetName())
					}
					return nil
				}, timeout, constants.PipelineRunPollingInterval).Should(Succeed(), fmt.Sprintf("timed out when waiting for the PipelineRun to start for the component %s/%s", testNamespace, customBranchComponentName))
			})
			It("should lead to a PaC init PR creation", func() {
				timeout = time.Second * 300
				interval = time.Second * 1

				Eventually(func() bool {
					prs, err := gitClient.ListPullRequests(helloWorldRepository)
					Expect(err).ShouldNot(HaveOccurred())

					for _, pr := range prs {
						if pr.SourceBranch == pacBranchName {
							prNumber = pr.Number
							prHeadSha = pr.HeadSHA
							return true
						}
					}
					return false
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("timed out when waiting for init PaC PR (branch name '%s') to be created in %s repository", pacBranchName, helloWorldComponentGitSourceRepoName))
			})
			It("the PipelineRun should eventually finish successfully", func() {
				Expect(f.AsKubeAdmin.HasController.WaitForComponentPipelineToBeFinished(component, "",
					f.AsKubeAdmin.TektonController, &has.RetryOptions{Retries: 2, Always: true}, plr)).To(Succeed())
				// in case the first pipelineRun attempt has failed and was retried, we need to update the git branch head ref
				prHeadSha = plr.Labels["pipelinesascode.tekton.dev/sha"]
			})
			It("image repo and robot account created successfully", func() {
				imageRepoName, err = f.AsKubeAdmin.ImageController.GetImageName(testNamespace, customBranchComponentName)
				Expect(err).ShouldNot(HaveOccurred(), "failed to read image repo for component %s", customBranchComponentName)
				Expect(imageRepoName).ShouldNot(BeEmpty(), "image repo name is empty")

				imageExist, err := build.DoesImageRepoExistInQuay(imageRepoName)
				Expect(err).ShouldNot(HaveOccurred(), "failed while checking if image repo exists in quay with error: %+v", err)
				Expect(imageExist).To(BeTrue(), "quay image does not exists")

				pullRobotAccountName, pushRobotAccountName, err = f.AsKubeAdmin.ImageController.GetRobotAccounts(testNamespace, customBranchComponentName)
				Expect(err).ShouldNot(HaveOccurred(), "failed to get robot account names")
				pullRobotAccountExist, err := build.DoesRobotAccountExistInQuay(pullRobotAccountName)
				Expect(err).ShouldNot(HaveOccurred(), "failed while checking if pull robot account exists in quay with error: %+v", err)
				Expect(pullRobotAccountExist).To(BeTrue(), "pull robot account does not exists in quay")
				pushRobotAccountExist, err := build.DoesRobotAccountExistInQuay(pushRobotAccountName)
				Expect(err).ShouldNot(HaveOccurred(), "failed while checking if push robot account exists in quay with error: %+v", err)
				Expect(pushRobotAccountExist).To(BeTrue(), "push robot account does not exists in quay")

			})
			It("floating tags are created successfully", func() {
				builtImage := build.GetBinaryImage(plr)
				Expect(builtImage).ToNot(BeEmpty(), "built image url is empty")
				builtImageRef, err := reference.Parse(builtImage)
				Expect(err).ShouldNot(HaveOccurred(),
					fmt.Sprintf("cannot parse image pullspec: %s", builtImage))
				for _, tagName := range additionalTags {
					_, err := build.GetImageTag(builtImageRef.Namespace, builtImageRef.Name, tagName)
					Expect(err).ShouldNot(HaveOccurred(),
						fmt.Sprintf("failed to get tag %s from image repo", tagName),
					)
				}
			})
			It("created image repo is public", func() {
				isPublic, err := build.IsImageRepoPublic(imageRepoName)
				Expect(err).ShouldNot(HaveOccurred(), fmt.Sprintf("failed while checking if the image repo %s is public", imageRepoName))
				Expect(isPublic).To(BeTrue(), fmt.Sprintf("Expected image repo '%s' to be changed to public, but it is private", imageRepoName))
			})
			It("image tag is updated successfully", func() {
				// check if the image tag exists in quay
				plr, err = f.AsKubeAdmin.HasController.GetComponentPipelineRun(customBranchComponentName, applicationName, testNamespace, "")
				Expect(err).ShouldNot(HaveOccurred())

				for _, p := range plr.Spec.Params {
					if p.Name == "output-image" {
						outputImage = p.Value.StringVal
					}
				}
				Expect(outputImage).ToNot(BeEmpty(), "output image %s of the component could not be found", outputImage)
				isExists, err := build.DoesTagExistsInQuay(outputImage)
				Expect(err).ShouldNot(HaveOccurred(), "error while checking if the output image %s exists in quay", outputImage)
				Expect(isExists).To(BeTrue(), "image tag does not exists in quay")
			})

			It("should ensure pruning labels are set", func() {
				plr, err = f.AsKubeAdmin.HasController.GetComponentPipelineRun(customBranchComponentName, applicationName, testNamespace, "")
				Expect(err).ShouldNot(HaveOccurred())

				image, err := build.ImageFromPipelineRun(plr)
				Expect(err).ShouldNot(HaveOccurred())

				labels := image.Config.Config.Labels
				Expect(labels).ToNot(BeEmpty())

				expiration, ok := labels["quay.expires-after"]
				Expect(ok).To(BeTrue())
				Expect(expiration).To(Equal(utils.GetEnv(constants.IMAGE_TAG_EXPIRATION_ENV, constants.DefaultImageTagExpiration)))
			})
			It("eventually leads to the PipelineRun status report at Checks tab", func() {
				switch gitProvider {
				case git.GitHubProvider:
					expectedCheckRunName := fmt.Sprintf("%s-%s", customBranchComponentName, "on-pull-request")
					Expect(f.AsKubeAdmin.CommonController.Github.GetCheckRunConclusion(expectedCheckRunName, helloWorldComponentGitSourceRepoName, prHeadSha, prNumber)).To(Equal(constants.CheckrunConclusionSuccess))
				case git.GitLabProvider:
					expectedNote := fmt.Sprintf("%s-on-pull-request** has successfully validated your commit", customBranchComponentName)
					f.AsKubeAdmin.HasController.GitLab.ValidateNoteInMergeRequestComment(helloWorldComponentGitLabProjectID, expectedNote, prNumber)
				}
			})
		})

		When("the PaC init branch is updated", Label("build-custom-branch"), func() {
			var createdFileSHA string

			BeforeAll(func() {
				fileToCreatePath := fmt.Sprintf(".tekton/%s-readme.md", customBranchComponentName)

				createdFile, err := gitClient.CreateFile(helloWorldRepository, fileToCreatePath, fmt.Sprintf("test PaC branch %s update", pacBranchName), pacBranchName)
				Expect(err).ShouldNot(HaveOccurred())

				createdFileSHA = createdFile.CommitSHA
				GinkgoWriter.Println("created file sha:", createdFileSHA)
			})

			It("eventually leads to triggering another PipelineRun", func() {
				timeout = time.Minute * 5

				Eventually(func() error {
					plr, err = f.AsKubeAdmin.HasController.GetComponentPipelineRun(customBranchComponentName, applicationName, testNamespace, createdFileSHA)
					if err != nil {
						GinkgoWriter.Printf("PipelineRun has not been created yet for the component %s/%s\n", testNamespace, customBranchComponentName)
						return err
					}
					if !plr.HasStarted() {
						return fmt.Errorf("pipelinerun %s/%s hasn't started yet", plr.GetNamespace(), plr.GetName())
					}
					return nil
				}, timeout, constants.PipelineRunPollingInterval).Should(Succeed(), fmt.Sprintf("timed out when waiting for the PipelineRun to start for the component %s/%s", testNamespace, customBranchComponentName))
			})
			It("should lead to a PaC init PR update", func() {
				timeout = time.Second * 300
				interval = time.Second * 1

				Eventually(func() bool {
					prs, err := gitClient.ListPullRequests(helloWorldRepository)
					Expect(err).ShouldNot(HaveOccurred())

					for _, pr := range prs {
						if pr.SourceBranch == pacBranchName {
							Expect(prHeadSha).NotTo(Equal(pr.HeadSHA))
							prNumber = pr.Number
							prHeadSha = pr.HeadSHA
							return true
						}
					}
					return false
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("timed out when waiting for init PaC PR (branch name '%s') to be created in %s repository", pacBranchName, helloWorldComponentGitSourceRepoName))
			})
			It("PipelineRun should eventually finish", func() {
				Expect(f.AsKubeAdmin.HasController.WaitForComponentPipelineToBeFinished(component, createdFileSHA,
					f.AsKubeAdmin.TektonController, &has.RetryOptions{Retries: 2, Always: true}, plr)).To(Succeed())
				// in case the first pipelineRun attempt has failed and was retried, we need to update the git branch head ref
				createdFileSHA = plr.Labels["pipelinesascode.tekton.dev/sha"]
			})
			It("eventually leads to another update of a PR about the PipelineRun status report at Checks tab", func() {
				switch gitProvider {
				case git.GitHubProvider:
					expectedCheckRunName := fmt.Sprintf("%s-%s", customBranchComponentName, "on-pull-request")
					Expect(f.AsKubeAdmin.CommonController.Github.GetCheckRunConclusion(expectedCheckRunName, helloWorldComponentGitSourceRepoName, createdFileSHA, prNumber)).To(Equal(constants.CheckrunConclusionSuccess))
				case git.GitLabProvider:
					expectedNote := fmt.Sprintf("%s-on-pull-request** has successfully validated your commit", customBranchComponentName)
					f.AsKubeAdmin.HasController.GitLab.ValidateNoteInMergeRequestComment(helloWorldComponentGitLabProjectID, expectedNote, prNumber)
				}
			})
		})

		When("the PaC init branch is merged", Label("build-custom-branch"), func() {
			var mergeResult *git.PullRequest
			var mergeResultSha string

			BeforeAll(func() {
				Eventually(func() error {
					mergeResult, err = gitClient.MergePullRequest(helloWorldRepository, prNumber)
					return err
				}, time.Minute).Should(BeNil(), fmt.Sprintf("error when merging PaC pull request #%d in repo %s", prNumber, helloWorldComponentGitSourceRepoName))

				mergeResultSha = mergeResult.MergeCommitSHA
				GinkgoWriter.Println("merged result sha:", mergeResultSha)
			})

			It("eventually leads to triggering another PipelineRun", func() {
				timeout = time.Minute * 10

				Eventually(func() error {
					plr, err = f.AsKubeAdmin.HasController.GetComponentPipelineRun(customBranchComponentName, applicationName, testNamespace, mergeResultSha)
					if err != nil {
						GinkgoWriter.Printf("PipelineRun has not been created yet for the component %s/%s\n", testNamespace, customBranchComponentName)
						return err
					}
					if !plr.HasStarted() {
						return fmt.Errorf("pipelinerun %s/%s hasn't started yet", plr.GetNamespace(), plr.GetName())
					}
					return nil
				}, timeout, constants.PipelineRunPollingInterval).Should(Succeed(), fmt.Sprintf("timed out when waiting for the PipelineRun to start for the component %s/%s", testNamespace, customBranchComponentName))
			})

			It("pipelineRun should eventually finish", func() {
				Expect(f.AsKubeAdmin.HasController.WaitForComponentPipelineToBeFinished(component,
					mergeResultSha, f.AsKubeAdmin.TektonController, &has.RetryOptions{Retries: 2, Always: true}, plr)).To(Succeed())
				mergeResultSha = plr.Labels["pipelinesascode.tekton.dev/sha"]
			})

			It("does not have expiration set", func() {
				image, err := build.ImageFromPipelineRun(plr)
				Expect(err).ShouldNot(HaveOccurred())

				labels := image.Config.Config.Labels
				Expect(labels).ToNot(BeEmpty())

				expiration, ok := labels["quay.expires-after"]
				Expect(ok).To(BeFalse())
				Expect(expiration).To(BeEmpty())
			})

			It("After updating image visibility to private, it should not trigger another PipelineRun", func() {
				Expect(f.AsKubeAdmin.TektonController.DeleteAllPipelineRunsInASpecificNamespace(testNamespace)).To(Succeed())
				Eventually(func() error {
					_, err := f.AsKubeAdmin.ImageController.ChangeVisibilityToPrivate(testNamespace, applicationName, customBranchComponentName)
					if err != nil {
						GinkgoWriter.Printf("failed to change visibility to private with error %v\n", err)
						return err
					}
					return nil
				}, time.Second*20, time.Second*1).Should(Succeed(), fmt.Sprintf("timed out when trying to change visibility of the image repos to private in %s/%s", testNamespace, customBranchComponentName))

				GinkgoWriter.Printf("waiting for one minute and expecting to not trigger a PipelineRun")
				Consistently(func() bool {
					componentPipelineRun, _ := f.AsKubeAdmin.HasController.GetComponentPipelineRun(customBranchComponentName, applicationName, testNamespace, "")
					if componentPipelineRun != nil {
						GinkgoWriter.Printf("While waiting for no pipeline to be triggered, found Pipelinerun: %s\n", componentPipelineRun.GetName())
					}
					return componentPipelineRun == nil
				}, 2*time.Minute, constants.PipelineRunPollingInterval).Should(BeTrue(), fmt.Sprintf("expected no PipelineRun to be triggered for the component %s in %s namespace", customBranchComponentName, testNamespace))
			})
			It("image repo is updated to private", func() {
				isPublic, err := build.IsImageRepoPublic(imageRepoName)
				Expect(err).ShouldNot(HaveOccurred(), fmt.Sprintf("failed while checking if the image repo %s is private", imageRepoName))
				Expect(isPublic).To(BeFalse(), "Expected image repo to changed to private, but it is public")
			})
		})

		When("the component is removed and recreated (with the same name in the same namespace)", Label("build-custom-branch"), func() {
			var componentObj appservice.ComponentSpec

			BeforeAll(func() {
				Expect(f.AsKubeAdmin.HasController.DeleteComponent(customBranchComponentName, testNamespace, true)).To(Succeed())

				timeout = 1 * time.Minute
				interval = 1 * time.Second
				Eventually(func() bool {
					_, err := f.AsKubeAdmin.HasController.GetComponent(customBranchComponentName, testNamespace)
					return k8sErrors.IsNotFound(err)
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("timed out when waiting for the app %s/%s to be deleted", testNamespace, applicationName))
				// Check removal of image repo
				Eventually(func() (bool, error) {
					return build.DoesImageRepoExistInQuay(imageRepoName)
				}, timeout, interval).Should(BeFalse(), fmt.Sprintf("timed out when waiting for image repo %s to be deleted", imageRepoName))
				// Check removal of robot accounts
				Eventually(func() (bool, error) {
					pullRobotAccountExists, err := build.DoesRobotAccountExistInQuay(pullRobotAccountName)
					if err != nil {
						return false, err
					}
					pushRobotAccountExists, err := build.DoesRobotAccountExistInQuay(pushRobotAccountName)
					if err != nil {
						return false, err
					}
					return pullRobotAccountExists || pushRobotAccountExists, nil
				}, timeout, interval).Should(BeFalse(), fmt.Sprintf("timed out when checking if robot accounts %s and %s got deleted", pullRobotAccountName, pushRobotAccountName))
			})

			BeforeAll(func() {
				componentObj = appservice.ComponentSpec{
					ComponentName: customBranchComponentName,
					Source: appservice.ComponentSource{
						ComponentSourceUnion: appservice.ComponentSourceUnion{
							GitSource: &appservice.GitSource{
								URL:           helloWorldComponentGitSourceURL,
								Revision:      componentBaseBranchName,
								DockerfileURL: constants.DockerFilePath,
							},
						},
					},
				}

				_, err = f.AsKubeAdmin.HasController.CreateComponent(componentObj, testNamespace, "", "", applicationName, false, utils.MergeMaps(utils.MergeMaps(constants.ComponentPaCRequestAnnotation, constants.ImageControllerAnnotationRequestPublicRepo), buildPipelineAnnotation))
				Expect(err).ShouldNot(HaveOccurred())
			})

			It("should no longer lead to a creation of a PaC PR", func() {
				timeout = time.Second * 10
				interval = time.Second * 2
				Consistently(func() error {
					prs, err := gitClient.ListPullRequests(helloWorldRepository)
					Expect(err).ShouldNot(HaveOccurred())

					for _, pr := range prs {
						if pr.SourceBranch == pacBranchName {
							return fmt.Errorf("did not expect a new PR created in %s repository after initial PaC configuration was already merged for the same component name and a namespace", helloWorldRepository)
						}
					}
					return nil
				}, timeout, interval).Should(BeNil())
			})
		})
	},
		Entry("github", git.GitHubProvider, "gh"),
		Entry("gitlab", git.GitLabProvider, "gl"),
	)

	Describe("test pac with multiple components using same repository", Ordered, Label("pac-build", "multi-component"), func() {
		var applicationName, testNamespace, multiComponentBaseBranchName, multiComponentPRBranchName, mergeResultSha string
		var pacBranchNames []string
		var prNumber int
		var mergeResult *github.PullRequestMergeResult
		var timeout time.Duration
		var buildPipelineAnnotation map[string]string

		BeforeAll(func() {
			if os.Getenv(constants.SKIP_PAC_TESTS_ENV) == "true" {
				Skip("Skipping this test due to configuration issue with Spray proxy")
			}
			f, err = framework.NewFramework(utils.GetGeneratedNamespace("build-e2e"))
			Expect(err).NotTo(HaveOccurred())
			testNamespace = f.UserNamespace

			if utils.IsPrivateHostname(f.OpenshiftConsoleHost) {
				Skip("Using private cluster (not reachable from Github), skipping...")
			}

			applicationName = fmt.Sprintf("build-suite-positive-mc-%s", util.GenerateRandomString(4))
			_, err = f.AsKubeAdmin.HasController.CreateApplication(applicationName, testNamespace)
			Expect(err).NotTo(HaveOccurred())

			multiComponentBaseBranchName = fmt.Sprintf("multi-component-base-%s", util.GenerateRandomString(6))
			err = f.AsKubeAdmin.CommonController.Github.CreateRef(multiComponentGitSourceRepoName, multiComponentDefaultBranch, multiComponentGitRevision, multiComponentBaseBranchName)
			Expect(err).ShouldNot(HaveOccurred())

			//Branch for creating pull request
			multiComponentPRBranchName = fmt.Sprintf("%s-%s", "pr-branch", util.GenerateRandomString(6))

			// get the build pipeline bundle annotation
			buildPipelineAnnotation = build.GetBuildPipelineBundleAnnotation(constants.DockerBuild)

		})

		AfterAll(func() {
			if !CurrentSpecReport().Failed() {
				Expect(f.AsKubeAdmin.HasController.DeleteAllComponentsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
				Expect(f.AsKubeAdmin.HasController.DeleteAllApplicationsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
			}

			// Delete new branches created by PaC and a testing branch used as a component's base branch
			for _, pacBranchName := range pacBranchNames {
				err = f.AsKubeAdmin.CommonController.Github.DeleteRef(multiComponentGitSourceRepoName, pacBranchName)
				if err != nil {
					Expect(err.Error()).To(ContainSubstring("Reference does not exist"))
				}
			}
			// Delete the created base branch
			err = f.AsKubeAdmin.CommonController.Github.DeleteRef(multiComponentGitSourceRepoName, multiComponentBaseBranchName)
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("Reference does not exist"))
			}
			// Delete the created pr branch
			err = f.AsKubeAdmin.CommonController.Github.DeleteRef(multiComponentGitSourceRepoName, multiComponentPRBranchName)
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("Reference does not exist"))
			}
		})

		When("components are created in same namespace", func() {
			var component *appservice.Component

			for _, contextDir := range multiComponentContextDirs {
				contextDir := contextDir
				componentName := fmt.Sprintf("%s-%s", contextDir, util.GenerateRandomString(6))
				pacBranchName := constants.PaCPullRequestBranchPrefix + componentName
				pacBranchNames = append(pacBranchNames, pacBranchName)

				It(fmt.Sprintf("creates component with context directory %s", contextDir), func() {
					componentObj := appservice.ComponentSpec{
						ComponentName: componentName,
						Application:   applicationName,
						Source: appservice.ComponentSource{
							ComponentSourceUnion: appservice.ComponentSourceUnion{
								GitSource: &appservice.GitSource{
									URL:           multiComponentGitHubURL,
									Revision:      multiComponentBaseBranchName,
									Context:       contextDir,
									DockerfileURL: constants.DockerFilePath,
								},
							},
						},
					}
					component, err = f.AsKubeAdmin.HasController.CreateComponent(componentObj, testNamespace, "", "", applicationName, false, utils.MergeMaps(utils.MergeMaps(constants.ComponentPaCRequestAnnotation, constants.ImageControllerAnnotationRequestPublicRepo), buildPipelineAnnotation))
					Expect(err).ShouldNot(HaveOccurred())
				})

				It(fmt.Sprintf("triggers a PipelineRun for component %s", componentName), func() {
					timeout = time.Minute * 5
					Eventually(func() error {
						pr, err := f.AsKubeAdmin.HasController.GetComponentPipelineRun(componentName, applicationName, testNamespace, "")
						if err != nil {
							GinkgoWriter.Printf("PipelineRun has not been created yet for the component %s/%s\n", testNamespace, componentName)
							return err
						}
						if !pr.HasStarted() {
							return fmt.Errorf("pipelinerun %s/%s hasn't started yet", pr.GetNamespace(), pr.GetName())
						}
						return nil
					}, timeout, constants.PipelineRunPollingInterval).Should(Succeed(), fmt.Sprintf("timed out when waiting for the PipelineRun to start for the component %s/%s", componentName, testNamespace))
				})

				It(fmt.Sprintf("should lead to a PaC PR creation for component %s", componentName), func() {
					timeout = time.Second * 300
					interval := time.Second * 1

					Eventually(func() bool {
						prs, err := f.AsKubeAdmin.CommonController.Github.ListPullRequests(multiComponentGitSourceRepoName)
						Expect(err).ShouldNot(HaveOccurred())

						for _, pr := range prs {
							if pr.Head.GetRef() == pacBranchName {
								prNumber = pr.GetNumber()
								return true
							}
						}
						return false
					}, timeout, interval).Should(BeTrue(), fmt.Sprintf("timed out when waiting for PaC PR (branch name '%s') to be created in %s repository", pacBranchName, multiComponentGitSourceRepoName))
				})

				It(fmt.Sprintf("the PipelineRun should eventually finish successfully for component %s", componentName), func() {
					Expect(f.AsKubeAdmin.HasController.WaitForComponentPipelineToBeFinished(component, "",
						f.AsKubeAdmin.TektonController, &has.RetryOptions{Retries: 2, Always: true}, nil)).To(Succeed())
				})

				It("merging the PR should be successful", func() {
					Eventually(func() error {
						mergeResult, err = f.AsKubeAdmin.CommonController.Github.MergePullRequest(multiComponentGitSourceRepoName, prNumber)
						return err
					}, time.Minute).Should(BeNil(), fmt.Sprintf("error when merging PaC pull request #%d in repo %s", prNumber, multiComponentGitSourceRepoName))

					mergeResultSha = mergeResult.GetSHA()
					GinkgoWriter.Printf("merged result sha: %s for PR #%d\n", mergeResultSha, prNumber)

				})
				It("leads to triggering on push PipelineRun", func() {
					timeout = time.Minute * 5

					Eventually(func() error {
						pipelineRun, err := f.AsKubeAdmin.HasController.GetComponentPipelineRun(componentName, applicationName, testNamespace, mergeResultSha)
						if err != nil {
							GinkgoWriter.Printf("Push PipelineRun has not been created yet for the component %s/%s\n", testNamespace, componentName)
							return err
						}
						if !pipelineRun.HasStarted() {
							return fmt.Errorf("push pipelinerun %s/%s hasn't started yet", pipelineRun.GetNamespace(), pipelineRun.GetName())
						}
						return nil
					}, timeout, constants.PipelineRunPollingInterval).Should(Succeed(), fmt.Sprintf("timed out when waiting for the PipelineRun to start for the component %s/%s", testNamespace, componentName))
				})
			}
			It("only one component is changed", func() {
				//Delete all the pipelineruns in the namespace before sending PR
				Expect(f.AsKubeAdmin.TektonController.DeleteAllPipelineRunsInASpecificNamespace(testNamespace)).To(Succeed())
				//Create the ref, add the file and create the PR
				err = f.AsKubeAdmin.CommonController.Github.CreateRef(multiComponentGitSourceRepoName, multiComponentDefaultBranch, mergeResultSha, multiComponentPRBranchName)
				Expect(err).ShouldNot(HaveOccurred())
				fileToCreatePath := fmt.Sprintf("%s/sample-file.txt", multiComponentContextDirs[0])
				createdFileSha, err := f.AsKubeAdmin.CommonController.Github.CreateFile(multiComponentGitSourceRepoName, fileToCreatePath, fmt.Sprintf("sample test file inside %s", multiComponentContextDirs[0]), multiComponentPRBranchName)
				Expect(err).ShouldNot(HaveOccurred(), fmt.Sprintf("error while creating file: %s", fileToCreatePath))
				pr, err := f.AsKubeAdmin.CommonController.Github.CreatePullRequest(multiComponentGitSourceRepoName, "sample pr title", "sample pr body", multiComponentPRBranchName, multiComponentBaseBranchName)
				Expect(err).ShouldNot(HaveOccurred())
				GinkgoWriter.Printf("PR #%d got created with sha %s\n", pr.GetNumber(), createdFileSha.GetSHA())
			})
			It("only related pipelinerun should be triggered", func() {
				Eventually(func() error {
					pipelineRuns, err := f.AsKubeAdmin.HasController.GetAllPipelineRunsForApplication(applicationName, testNamespace)
					if err != nil {
						GinkgoWriter.Println("on pull PiplelineRun has not been created yet for the PR")
						return err
					}
					if len(pipelineRuns.Items) != 1 || !strings.HasPrefix(pipelineRuns.Items[0].Name, multiComponentContextDirs[0]) {
						return fmt.Errorf("pipelinerun created in the namespace %s is not as expected, got pipelineruns %v", testNamespace, pipelineRuns.Items)
					}
					return nil
				}, time.Minute*5, constants.PipelineRunPollingInterval).Should(Succeed(), "timeout while waiting for PR pipeline to start")
			})
		})
		When("a components is created with same git url in different namespace", func() {
			var namespace, appName, compName string
			var fw *framework.Framework

			BeforeAll(func() {
				fw, err = framework.NewFramework(utils.GetGeneratedNamespace("build-e2e"))
				Expect(err).NotTo(HaveOccurred())
				namespace = fw.UserNamespace

				appName = fmt.Sprintf("build-suite-negative-mc-%s", util.GenerateRandomString(4))
				_, err = f.AsKubeAdmin.HasController.CreateApplication(appName, namespace)
				Expect(err).NotTo(HaveOccurred())

				compName = fmt.Sprintf("%s-%s", multiComponentContextDirs[0], util.GenerateRandomString(6))

				componentObj := appservice.ComponentSpec{
					ComponentName: compName,
					Application:   appName,
					Source: appservice.ComponentSource{
						ComponentSourceUnion: appservice.ComponentSourceUnion{
							GitSource: &appservice.GitSource{
								URL:           multiComponentGitHubURL,
								Revision:      multiComponentBaseBranchName,
								Context:       multiComponentContextDirs[0],
								DockerfileURL: constants.DockerFilePath,
							},
						},
					},
				}
				_, err = fw.AsKubeAdmin.HasController.CreateComponent(componentObj, namespace, "", "", appName, false, utils.MergeMaps(utils.MergeMaps(constants.ComponentPaCRequestAnnotation, constants.ImageControllerAnnotationRequestPublicRepo), buildPipelineAnnotation))
				Expect(err).ShouldNot(HaveOccurred())

			})

			AfterAll(func() {
				if !CurrentSpecReport().Failed() {
					Expect(f.AsKubeAdmin.HasController.DeleteAllComponentsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
					Expect(f.AsKubeAdmin.HasController.DeleteAllApplicationsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
				}
			})

			It("should fail to configure PaC for the component", func() {
				var buildStatus *controllers.BuildStatus

				Eventually(func() (bool, error) {
					component, err := fw.AsKubeAdmin.HasController.GetComponent(compName, namespace)
					if err != nil {
						GinkgoWriter.Printf("error while getting the component: %v\n", err)
						return false, err
					}

					buildStatusAnnotationValue := component.Annotations[controllers.BuildStatusAnnotationName]
					GinkgoWriter.Printf(buildStatusAnnotationValueLoggingFormat, buildStatusAnnotationValue)
					statusBytes := []byte(buildStatusAnnotationValue)

					err = json.Unmarshal(statusBytes, &buildStatus)
					if err != nil {
						GinkgoWriter.Printf("cannot unmarshal build status from component annotation: %v\n", err)
						return false, err
					}

					GinkgoWriter.Printf("build status: %+v\n", buildStatus.PaC)

					return buildStatus.PaC != nil && buildStatus.PaC.State == "error" && strings.Contains(buildStatus.PaC.ErrMessage, "Git repository is already handled by Pipelines as Code"), nil
				}, time.Minute*2, time.Second*2).Should(BeTrue(), "build status is unexpected")

			})

		})

	})
	Describe("test build secret lookup", Label("pac-build", "secret-lookup"), Ordered, func() {
		var testNamespace, applicationName, firstComponentBaseBranchName, secondComponentBaseBranchName, firstComponentName, secondComponentName, firstPacBranchName, secondPacBranchName string
		var buildPipelineAnnotation map[string]string
		BeforeAll(func() {
			if os.Getenv(constants.SKIP_PAC_TESTS_ENV) == "true" {
				Skip("Skipping this test due to configuration issue with Spray proxy")
			}
			f, err = framework.NewFramework(utils.GetGeneratedNamespace("build-e2e"))
			Expect(err).NotTo(HaveOccurred())
			testNamespace = f.UserNamespace

			applicationName = fmt.Sprintf("build-secret-lookup-%s", util.GenerateRandomString(4))
			_, err = f.AsKubeAdmin.HasController.CreateApplication(applicationName, testNamespace)
			Expect(err).NotTo(HaveOccurred())

			// Update the default github org
			f.AsKubeAdmin.CommonController.Github.UpdateGithubOrg(noAppOrgName)

			firstComponentBaseBranchName = fmt.Sprintf("component-one-base-%s", util.GenerateRandomString(6))
			err = f.AsKubeAdmin.CommonController.Github.CreateRef(secretLookupGitSourceRepoOneName, secretLookupDefaultBranchOne, secretLookupGitRevisionOne, firstComponentBaseBranchName)
			Expect(err).ShouldNot(HaveOccurred())

			secondComponentBaseBranchName = fmt.Sprintf("component-two-base-%s", util.GenerateRandomString(6))
			err = f.AsKubeAdmin.CommonController.Github.CreateRef(secretLookupGitSourceRepoTwoName, secretLookupDefaultBranchTwo, secretLookupGitRevisionTwo, secondComponentBaseBranchName)
			Expect(err).ShouldNot(HaveOccurred())

			// use custom bundle if env defined
			// get the build pipeline bundle annotation
			buildPipelineAnnotation = build.GetBuildPipelineBundleAnnotation(constants.DockerBuild)

		})

		AfterAll(func() {
			if !CurrentSpecReport().Failed() {
				Expect(f.AsKubeAdmin.HasController.DeleteAllComponentsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
				Expect(f.AsKubeAdmin.HasController.DeleteAllApplicationsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
			}

			// Delete new branches created by PaC
			err = f.AsKubeAdmin.CommonController.Github.DeleteRef(secretLookupGitSourceRepoOneName, firstPacBranchName)
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("Reference does not exist"))
			}
			err = f.AsKubeAdmin.CommonController.Github.DeleteRef(secretLookupGitSourceRepoTwoName, secondPacBranchName)
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("Reference does not exist"))
			}

			// Delete the created first component base branch
			err = f.AsKubeAdmin.CommonController.Github.DeleteRef(secretLookupGitSourceRepoOneName, firstComponentBaseBranchName)
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("Reference does not exist"))
			}
			// Delete the created second component base branch
			err = f.AsKubeAdmin.CommonController.Github.DeleteRef(secretLookupGitSourceRepoTwoName, secondComponentBaseBranchName)
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("Reference does not exist"))
			}

			// Delete created webhook from GitHub
			err = build.CleanupWebhooks(f, secretLookupGitSourceRepoTwoName)
			if err != nil {
				Expect(err.Error()).To(ContainSubstring("404 Not Found"))
			}

		})
		When("two secrets are created", func() {
			BeforeAll(func() {
				// create the correct build secret for second component
				secretName1 := "build-secret-1"
				secretAnnotations := map[string]string{
					"appstudio.redhat.com/scm.repository": noAppOrgName + "/" + secretLookupGitSourceRepoTwoName,
				}
				token := os.Getenv("GITHUB_TOKEN")
				err = createBuildSecret(f, secretName1, secretAnnotations, token)
				Expect(err).ShouldNot(HaveOccurred())

				// create incorrect build-secret for the first component
				secretName2 := "build-secret-2"
				dummyToken := "ghp_dummy_secret"
				err = createBuildSecret(f, secretName2, nil, dummyToken)
				Expect(err).ShouldNot(HaveOccurred())

				// component names and pac branch names
				firstComponentName = fmt.Sprintf("%s-%s", "component-one", util.GenerateRandomString(4))
				secondComponentName = fmt.Sprintf("%s-%s", "component-two", util.GenerateRandomString(4))
				firstPacBranchName = constants.PaCPullRequestBranchPrefix + firstComponentName
				secondPacBranchName = constants.PaCPullRequestBranchPrefix + secondComponentName
			})

			It("creates first component", func() {
				componentObj1 := appservice.ComponentSpec{
					ComponentName: firstComponentName,
					Application:   applicationName,
					Source: appservice.ComponentSource{
						ComponentSourceUnion: appservice.ComponentSourceUnion{
							GitSource: &appservice.GitSource{
								URL:           secretLookupComponentOneGitSourceURL,
								Revision:      firstComponentBaseBranchName,
								DockerfileURL: constants.DockerFilePath,
							},
						},
					},
				}
				_, err := f.AsKubeAdmin.HasController.CreateComponent(componentObj1, testNamespace, "", "", applicationName, false, utils.MergeMaps(utils.MergeMaps(constants.ComponentPaCRequestAnnotation, constants.ImageControllerAnnotationRequestPublicRepo), buildPipelineAnnotation))
				Expect(err).ShouldNot(HaveOccurred())
			})
			It("creates second component", func() {
				componentObj2 := appservice.ComponentSpec{
					ComponentName: secondComponentName,
					Application:   applicationName,
					Source: appservice.ComponentSource{
						ComponentSourceUnion: appservice.ComponentSourceUnion{
							GitSource: &appservice.GitSource{
								URL:           secretLookupComponentTwoGitSourceURL,
								Revision:      secondComponentBaseBranchName,
								DockerfileURL: constants.DockerFilePath,
							},
						},
					},
				}
				_, err := f.AsKubeAdmin.HasController.CreateComponent(componentObj2, testNamespace, "", "", applicationName, false, utils.MergeMaps(utils.MergeMaps(constants.ComponentPaCRequestAnnotation, constants.ImageControllerAnnotationRequestPublicRepo), buildPipelineAnnotation))
				Expect(err).ShouldNot(HaveOccurred())
			})

			It("check first component annotation has errors", func() {
				buildStatus := &controllers.BuildStatus{}
				Eventually(func() (bool, error) {
					component, err := f.AsKubeAdmin.HasController.GetComponent(firstComponentName, testNamespace)
					if err != nil {
						return false, err
					} else if component == nil {
						return false, fmt.Errorf("got component as nil after getting component %s in namespace %s", firstComponentName, testNamespace)
					}
					buildStatusAnnotationValue := component.Annotations[controllers.BuildStatusAnnotationName]
					GinkgoWriter.Printf(buildStatusAnnotationValueLoggingFormat, buildStatusAnnotationValue)
					statusBytes := []byte(buildStatusAnnotationValue)
					err = json.Unmarshal(statusBytes, buildStatus)
					if err != nil {
						return false, err
					}
					return buildStatus.PaC != nil && buildStatus.PaC.State == "error" && strings.Contains(buildStatus.PaC.ErrMessage, "Access token is unrecognizable by GitHub"), nil
				}, time.Minute*2, 5*time.Second).Should(BeTrue(), "failed while checking build status for component %q is correct", firstComponentName)
			})

			It(fmt.Sprintf("triggered PipelineRun is for component %s", secondComponentName), func() {
				timeout = time.Minute * 5
				Eventually(func() error {
					pr, err := f.AsKubeAdmin.HasController.GetComponentPipelineRun(secondComponentName, applicationName, testNamespace, "")
					if err != nil {
						GinkgoWriter.Printf("PipelineRun has not been created yet for the component %s/%s\n", testNamespace, secondComponentName)
						return err
					}
					if !pr.HasStarted() {
						return fmt.Errorf("pipelinerun %s/%s hasn't started yet", pr.GetNamespace(), pr.GetName())
					}
					return nil
				}, timeout, constants.PipelineRunPollingInterval).Should(Succeed(), fmt.Sprintf("timed out when waiting for the PipelineRun to start for the component %s/%s", secondComponentName, testNamespace))
			})

			It("check only one pipelinerun should be triggered", func() {
				// Waiting for 2 minute to see if only one pipelinerun is triggered
				Consistently(func() (bool, error) {
					pipelineRuns, err := f.AsKubeAdmin.HasController.GetAllPipelineRunsForApplication(applicationName, testNamespace)
					if err != nil {
						return false, err
					}
					if len(pipelineRuns.Items) != 1 {
						return false, fmt.Errorf("plr count in the namespace %s is not one, got pipelineruns %v", testNamespace, pipelineRuns.Items)
					}
					return true, nil
				}, time.Minute*2, constants.PipelineRunPollingInterval).Should(BeTrue(), "timeout while checking if any more pipelinerun is triggered")
			})
			It("when second component is deleted, pac pr branch should not exist in the repo", Pending, func() {
				timeout = time.Second * 60
				interval = time.Second * 1
				Expect(f.AsKubeAdmin.HasController.DeleteComponent(secondComponentName, testNamespace, true)).To(Succeed())
				Eventually(func() bool {
					exists, err := f.AsKubeAdmin.CommonController.Github.ExistsRef(secretLookupGitSourceRepoTwoName, secondPacBranchName)
					Expect(err).ShouldNot(HaveOccurred())
					return exists
				}, timeout, interval).Should(BeFalse(), fmt.Sprintf("timed out when waiting for the branch %s to be deleted from %s repository", secondPacBranchName, secretLookupGitSourceRepoTwoName))
			})
		})
	})
	Describe("test build annotations", Label("annotations"), Ordered, func() {
		var testNamespace, componentName, applicationName string
		var componentObj appservice.ComponentSpec
		var component *appservice.Component
		var buildPipelineAnnotation map[string]string
		invalidAnnotation := "foo"

		BeforeAll(func() {
			f, err = framework.NewFramework(utils.GetGeneratedNamespace("build-e2e"))
			Expect(err).ShouldNot(HaveOccurred())
			testNamespace = f.UserNamespace

			applicationName = fmt.Sprintf("build-suite-test-application-%s", util.GenerateRandomString(4))
			_, err = f.AsKubeAdmin.HasController.CreateApplication(applicationName, testNamespace)
			Expect(err).NotTo(HaveOccurred())

			componentName = fmt.Sprintf("%s-%s", "test-annotations", util.GenerateRandomString(6))

			// get the build pipeline bundle annotation
			buildPipelineAnnotation = build.GetBuildPipelineBundleAnnotation(constants.DockerBuild)

		})

		AfterAll(func() {
			if !CurrentSpecReport().Failed() {
				Expect(f.AsKubeAdmin.HasController.DeleteAllComponentsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
				Expect(f.AsKubeAdmin.HasController.DeleteAllApplicationsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
			}

		})

		When("component is created with invalid build request annotations", func() {

			invalidBuildAnnotation := map[string]string{
				controllers.BuildRequestAnnotationName: invalidAnnotation,
			}

			BeforeAll(func() {
				componentObj = appservice.ComponentSpec{
					ComponentName: componentName,
					Application:   applicationName,
					Source: appservice.ComponentSource{
						ComponentSourceUnion: appservice.ComponentSourceUnion{
							GitSource: &appservice.GitSource{
								URL:           annotationsTestGitHubURL,
								Revision:      annotationsTestRevision,
								DockerfileURL: constants.DockerFilePath,
							},
						},
					},
				}

				component, err = f.AsKubeAdmin.HasController.CreateComponent(componentObj, testNamespace, "", "", applicationName, false, utils.MergeMaps(invalidBuildAnnotation, buildPipelineAnnotation))
				Expect(component).ToNot(BeNil())
				Expect(err).ShouldNot(HaveOccurred())
			})

			It("handles invalid request annotation", func() {

				expectedInvalidAnnotationMessage := fmt.Sprintf("unexpected build request: %s", invalidAnnotation)

				// Waiting for 1 minute to see if any pipelinerun is triggered
				Consistently(func() bool {
					_, err := f.AsKubeAdmin.HasController.GetComponentPipelineRun(componentName, applicationName, testNamespace, "")
					Expect(err).To(HaveOccurred())
					return strings.Contains(err.Error(), "no pipelinerun found")
				}, time.Minute*1, constants.PipelineRunPollingInterval).Should(BeTrue(), "timeout while checking if any pipelinerun is triggered")

				buildStatus := &controllers.BuildStatus{}
				Eventually(func() error {
					component, err = f.AsKubeAdmin.HasController.GetComponent(componentName, testNamespace)
					if err != nil {
						return err
					} else if component == nil {
						return fmt.Errorf("got component as nil after getting component %s in namespace %s", componentName, testNamespace)
					}
					buildStatusAnnotationValue := component.Annotations[controllers.BuildStatusAnnotationName]
					GinkgoWriter.Printf(buildStatusAnnotationValueLoggingFormat, buildStatusAnnotationValue)
					statusBytes := []byte(buildStatusAnnotationValue)
					err = json.Unmarshal(statusBytes, buildStatus)
					if err != nil {
						return err
					}
					if !strings.Contains(buildStatus.Message, expectedInvalidAnnotationMessage) {
						return fmt.Errorf("build status message is not as expected, got: %q, expected: %q", buildStatus.Message, expectedInvalidAnnotationMessage)
					}
					return nil
				}, time.Minute*2, 2*time.Second).Should(Succeed(), "failed while checking build status message for component %q is correct after setting invalid annotations", componentName)
			})
		})
	})

	Describe("Creating component with container image source", Ordered, func() {
		var applicationName, componentName, testNamespace string
		var timeout time.Duration
		var buildPipelineAnnotation map[string]string

		BeforeAll(func() {
			applicationName = fmt.Sprintf("test-app-%s", util.GenerateRandomString(4))
			f, err = framework.NewFramework(utils.GetGeneratedNamespace("build-e2e"))
			Expect(err).NotTo(HaveOccurred())
			testNamespace = f.UserNamespace

			_, err = f.AsKubeAdmin.HasController.CreateApplication(applicationName, testNamespace)
			Expect(err).NotTo(HaveOccurred())

			componentName = fmt.Sprintf("build-suite-test-component-image-source-%s", util.GenerateRandomString(6))
			outputContainerImage := ""
			timeout = time.Second * 10
			// Create a component with containerImageSource being defined
			component := appservice.ComponentSpec{
				ComponentName:  fmt.Sprintf("build-suite-test-component-image-source-%s", util.GenerateRandomString(6)),
				ContainerImage: containerImageSource,
			}
			_, err = f.AsKubeAdmin.HasController.CreateComponent(component, testNamespace, outputContainerImage, "", applicationName, true, buildPipelineAnnotation)
			Expect(err).ShouldNot(HaveOccurred())

			// get the build pipeline bundle annotation
			buildPipelineAnnotation = build.GetBuildPipelineBundleAnnotation(constants.DockerBuild)
		})

		AfterAll(func() {
			if !CurrentSpecReport().Failed() {
				Expect(f.AsKubeAdmin.HasController.DeleteAllComponentsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
				Expect(f.AsKubeAdmin.HasController.DeleteAllApplicationsInASpecificNamespace(testNamespace, time.Minute*5)).To(Succeed())
			}
		})

		It("should not trigger a PipelineRun", func() {
			Consistently(func() bool {
				_, err := f.AsKubeAdmin.HasController.GetComponentPipelineRun(componentName, applicationName, testNamespace, "")
				Expect(err).To(HaveOccurred())
				return strings.Contains(err.Error(), "no pipelinerun found")
			}, timeout, constants.PipelineRunPollingInterval).Should(BeTrue(), fmt.Sprintf("expected no PipelineRun to be triggered for the component %s in %s namespace", componentName, testNamespace))
		})
	})

	DescribeTableSubtree("test of component update with renovate", Ordered, Label("renovate", "multi-component"), func(gitProvider git.GitProvider, gitPrefix string) {
		type multiComponent struct {
			repoName        string
			baseBranch      string
			componentBranch string
			baseRevision    string
			componentName   string
			gitRepo         string
			pacBranchName   string
			component       *appservice.Component
		}

		ChildComponentDef := multiComponent{repoName: componentDependenciesChildRepoName, baseRevision: componentDependenciesChildGitRevision, baseBranch: componentDependenciesChildDefaultBranch}
		ParentComponentDef := multiComponent{repoName: componentDependenciesParentRepoName, baseRevision: componentDependenciesParentGitRevision, baseBranch: componentDependenciesParentDefaultBranch}
		components := []*multiComponent{&ChildComponentDef, &ParentComponentDef}
		var applicationName, testNamespace, mergeResultSha, imageRepoName string
		var prNumber int
		var mergeResult *git.PullRequest
		var timeout time.Duration
		var parentFirstDigest string
		var parentPostPacMergeDigest string
		var parentImageNameWithNoDigest string
		const distributionRepository = "quay.io/redhat-appstudio-qe/release-repository"
		quayOrg := utils.GetEnv("DEFAULT_QUAY_ORG", "")
		var parentRepository, childRepository string

		var managedNamespace string
		var buildPipelineAnnotation map[string]string

		var gitClient git.Client
		var componentDependenciesChildRepository string

		BeforeAll(func() {
			f, err = framework.NewFramework(utils.GetGeneratedNamespace("build-e2e"))
			Expect(err).NotTo(HaveOccurred())
			testNamespace = f.UserNamespace

			applicationName = fmt.Sprintf("build-suite-component-update-%s", util.GenerateRandomString(4))
			_, err = f.AsKubeAdmin.HasController.CreateApplication(applicationName, testNamespace)
			Expect(err).NotTo(HaveOccurred())
			branchString := util.GenerateRandomString(4)
			ParentComponentDef.componentBranch = fmt.Sprintf("multi-component-parent-base-%s", branchString)
			ChildComponentDef.componentBranch = fmt.Sprintf("multi-component-child-base-%s", branchString)
			switch gitProvider {
			case git.GitHubProvider:
				gitClient = git.NewGitHubClient(f.AsKubeAdmin.CommonController.Github)

				ParentComponentDef.gitRepo = fmt.Sprintf(githubUrlFormat, githubOrg, ParentComponentDef.repoName)
				parentRepository = ParentComponentDef.repoName

				ChildComponentDef.gitRepo = fmt.Sprintf(githubUrlFormat, githubOrg, ChildComponentDef.repoName)
				childRepository = ChildComponentDef.repoName

				componentDependenciesChildRepository = componentDependenciesChildRepoName
			case git.GitLabProvider:
				gitClient = git.NewGitlabClient(f.AsKubeAdmin.CommonController.Gitlab)

				parentRepository = fmt.Sprintf("%s/%s", gitlabOrg, ParentComponentDef.repoName)
				ParentComponentDef.gitRepo = fmt.Sprintf(gitlabUrlFormat, parentRepository)

				childRepository = fmt.Sprintf("%s/%s", gitlabOrg, ChildComponentDef.repoName)
				ChildComponentDef.gitRepo = fmt.Sprintf(gitlabUrlFormat, childRepository)

				componentDependenciesChildRepository = fmt.Sprintf("%s/%s", gitlabOrg, componentDependenciesChildRepoName)
			}
			ParentComponentDef.componentName = fmt.Sprintf("%s-multi-component-parent-%s", gitPrefix, branchString)
			ChildComponentDef.componentName = fmt.Sprintf("%s-multi-component-child-%s", gitPrefix, branchString)
			ParentComponentDef.pacBranchName = constants.PaCPullRequestBranchPrefix + ParentComponentDef.componentName
			ChildComponentDef.pacBranchName = constants.PaCPullRequestBranchPrefix + ChildComponentDef.componentName

			err = gitClient.CreateBranch(parentRepository, ParentComponentDef.baseBranch, ParentComponentDef.baseRevision, ParentComponentDef.componentBranch)
			Expect(err).ShouldNot(HaveOccurred())

			err = gitClient.CreateBranch(childRepository, ChildComponentDef.baseBranch, ChildComponentDef.baseRevision, ChildComponentDef.componentBranch)
			Expect(err).ShouldNot(HaveOccurred())

			// Also setup a release namespace so we can test nudging of distribution repository images
			managedNamespace = testNamespace + "-managed"
			_, err = f.AsKubeAdmin.CommonController.CreateTestNamespace(managedNamespace)
			Expect(err).ShouldNot(HaveOccurred())

			// We just need the ReleaseAdmissionPlan to contain a mapping between component and distribution repositories
			data := struct {
				Mapping struct {
					Components []struct {
						Name       string
						Repository string
					}
				}
			}{}
			data.Mapping.Components = append(data.Mapping.Components, struct {
				Name       string
				Repository string
			}{Name: ParentComponentDef.componentName, Repository: distributionRepository})
			rawData, err := json.Marshal(&data)
			Expect(err).NotTo(HaveOccurred())

			GinkgoWriter.Printf("ReleaseAdmissionPlan data: %s", string(rawData))
			managedServiceAccount, err := f.AsKubeAdmin.CommonController.CreateServiceAccount("release-service-account", managedNamespace, nil, nil)
			Expect(err).NotTo(HaveOccurred())

			_, err = f.AsKubeAdmin.ReleaseController.CreateReleasePipelineRoleBindingForServiceAccount(managedNamespace, managedServiceAccount)
			Expect(err).NotTo(HaveOccurred())

			_, err = f.AsKubeAdmin.ReleaseController.CreateReleasePlanAdmission("demo", managedNamespace, "", f.UserNamespace, "demo", "release-service-account", []string{applicationName}, true, &tektonutils.PipelineRef{
				Resolver: "git",
				Params: []tektonutils.Param{
					{Name: "url", Value: constants.RELEASE_CATALOG_DEFAULT_URL},
					{Name: "revision", Value: constants.RELEASE_CATALOG_DEFAULT_REVISION},
					{Name: "pathInRepo", Value: "pipelines/managed/e2e/e2e.yaml"},
				}}, &runtime.RawExtension{Raw: rawData})
			Expect(err).NotTo(HaveOccurred())

			// get the build pipeline bundle annotation
			buildPipelineAnnotation = build.GetBuildPipelineBundleAnnotation(constants.DockerBuild)

			if gitProvider == git.GitLabProvider {
				gitlabToken := utils.GetEnv(constants.GITLAB_BOT_TOKEN_ENV, "")
				Expect(gitlabToken).ShouldNot(BeEmpty())

				secretAnnotations := map[string]string{}

				err = build.CreateGitlabBuildSecret(f, "pipelines-as-code-secret", secretAnnotations, gitlabToken)
				Expect(err).ShouldNot(HaveOccurred())
			}
		})

		AfterAll(func() {
			if !CurrentSpecReport().Failed() {
				Expect(f.AsKubeAdmin.HasController.DeleteComponent(ParentComponentDef.componentName, testNamespace, true)).To(Succeed())
				Expect(f.AsKubeAdmin.HasController.DeleteComponent(ChildComponentDef.componentName, testNamespace, true)).To(Succeed())
				Expect(f.AsKubeAdmin.HasController.DeleteApplication(applicationName, testNamespace, false)).To(Succeed())
			}
			Expect(f.AsKubeAdmin.CommonController.DeleteNamespace(managedNamespace)).ShouldNot(HaveOccurred())

			repositories := []string{childRepository, parentRepository}
			// Delete new branches created by renovate and a testing branch used as a component's base branch
			for i, c := range components {
				println("deleting branch " + c.componentBranch)
				err = gitClient.DeleteBranch(repositories[i], c.componentBranch)
				if err != nil {
					Expect(err.Error()).To(Or(ContainSubstring("Reference does not exist"), ContainSubstring("Branch Not Found")))
				}
				err = gitClient.DeleteBranch(repositories[i], c.pacBranchName)
				if err != nil {
					Expect(err.Error()).To(Or(ContainSubstring("Reference does not exist"), ContainSubstring("Branch Not Found")))
				}
				// Cleanup webhooks
				Expect(gitClient.CleanupWebhooks(repositories[i], f.ClusterAppDomain)).To(Succeed())
			}
		})

		When("components are created in same namespace", func() {

			It("creates component with nudges", func() {
				for _, comp := range components {
					componentObj := appservice.ComponentSpec{
						ComponentName: comp.componentName,
						Application:   applicationName,
						Source: appservice.ComponentSource{
							ComponentSourceUnion: appservice.ComponentSourceUnion{
								GitSource: &appservice.GitSource{
									URL:           comp.gitRepo,
									Revision:      comp.componentBranch,
									DockerfileURL: "Dockerfile",
								},
							},
						},
					}
					//make the parent repo nudge the child repo
					if comp.repoName == componentDependenciesParentRepoName {
						componentObj.BuildNudgesRef = []string{ChildComponentDef.componentName}
					}
					comp.component, err = f.AsKubeAdmin.HasController.CreateComponent(componentObj, testNamespace, "", "", applicationName, true, utils.MergeMaps(utils.MergeMaps(constants.ComponentPaCRequestAnnotation, constants.ImageControllerAnnotationRequestPublicRepo), buildPipelineAnnotation))
					Expect(err).ShouldNot(HaveOccurred())
				}
			})
			// Initial pipeline run, we need this so we have an initial image that we can then update
			It(fmt.Sprintf("triggers a PipelineRun for parent component %s", ParentComponentDef.componentName), func() {
				timeout = time.Minute * 5

				Eventually(func() error {
					pr, err := f.AsKubeAdmin.HasController.GetComponentPipelineRun(ParentComponentDef.componentName, applicationName, testNamespace, "")
					if err != nil {
						GinkgoWriter.Printf("PipelineRun has not been created yet for the component %s/%s\n", testNamespace, ParentComponentDef.componentName)
						return err
					}
					if !pr.HasStarted() {
						return fmt.Errorf("pipelinerun %s/%s hasn't started yet", pr.GetNamespace(), pr.GetName())
					}
					return nil
				}, timeout, constants.PipelineRunPollingInterval).Should(Succeed(), fmt.Sprintf("timed out when waiting for the PipelineRun to start for the component %s/%s", ParentComponentDef.componentName, testNamespace))
			})
			It(fmt.Sprintf("the PipelineRun should eventually finish successfully for parent component %s", ParentComponentDef.componentName), func() {
				Expect(f.AsKubeAdmin.HasController.WaitForComponentPipelineToBeFinished(ParentComponentDef.component, "", f.AsKubeAdmin.TektonController, &has.RetryOptions{Always: true, Retries: 2}, nil)).To(Succeed())
				pr, err := f.AsKubeAdmin.HasController.GetComponentPipelineRun(ParentComponentDef.component.GetName(), ParentComponentDef.component.Spec.Application, ParentComponentDef.component.GetNamespace(), "")
				Expect(err).ShouldNot(HaveOccurred())
				for _, result := range pr.Status.PipelineRunStatusFields.Results {
					if result.Name == "IMAGE_DIGEST" {
						parentFirstDigest = result.Value.StringVal
					}
				}
				Expect(parentFirstDigest).ShouldNot(BeEmpty())
			})

			It(fmt.Sprintf("the PipelineRun should eventually finish successfully for child component %s", ChildComponentDef.componentName), func() {
				Expect(f.AsKubeAdmin.HasController.WaitForComponentPipelineToBeFinished(ChildComponentDef.component, "", f.AsKubeAdmin.TektonController, &has.RetryOptions{Always: true, Retries: 2}, nil)).To(Succeed())
			})

			It(fmt.Sprintf("should lead to a PaC PR creation for child component %s", ChildComponentDef.componentName), func() {
				timeout = time.Second * 300
				interval := time.Second * 1

				Eventually(func() bool {
					prs, err := gitClient.ListPullRequests(childRepository)
					Expect(err).ShouldNot(HaveOccurred())

					for _, pr := range prs {
						if pr.SourceBranch == ChildComponentDef.pacBranchName {
							prNumber = pr.Number
							return true
						}
					}
					return false
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("timed out when waiting for PaC PR (branch name '%s') to be created in %s repository", ChildComponentDef.pacBranchName, ChildComponentDef.repoName))
			})

			It(fmt.Sprintf("Merging the PaC PR should be successful for child component %s", ChildComponentDef.componentName), func() {
				Eventually(func() error {
					mergeResult, err = gitClient.MergePullRequest(childRepository, prNumber)
					return err
				}, time.Minute).Should(BeNil(), fmt.Sprintf("error when merging PaC pull request #%d in repo %s", prNumber, ChildComponentDef.repoName))

				mergeResultSha = mergeResult.MergeCommitSHA
				GinkgoWriter.Printf("merged result sha: %s for PR #%d\n", mergeResultSha, prNumber)
			})
			// Now we have an initial image we create a dockerfile in the child that references this new image
			// This is the file that will be updated by the nudge
			It("create dockerfile and yaml manifest that references build and distribution repositories", func() {

				imageRepoName, err = f.AsKubeAdmin.ImageController.GetImageName(testNamespace, ParentComponentDef.componentName)
				Expect(err).ShouldNot(HaveOccurred(), "failed to read image repo for component %s", ParentComponentDef.componentName)
				Expect(imageRepoName).ShouldNot(BeEmpty(), "image repo name is empty")

				parentImageNameWithNoDigest = "quay.io/" + quayOrg + "/" + imageRepoName
				_, err = gitClient.CreateFile(childRepository, "Dockerfile.tmp", "FROM "+parentImageNameWithNoDigest+"@"+parentFirstDigest+"\nRUN echo hello\n", ChildComponentDef.pacBranchName)
				Expect(err).ShouldNot(HaveOccurred())

				_, err = gitClient.CreateFile(childRepository, "manifest.yaml", "image: "+distributionRepository+"@"+parentFirstDigest, ChildComponentDef.pacBranchName)
				Expect(err).ShouldNot(HaveOccurred())

				_, err = gitClient.CreatePullRequest(childRepository, "updated to build repo image", "update to build repo image", ChildComponentDef.pacBranchName, ChildComponentDef.componentBranch)
				Expect(err).ShouldNot(HaveOccurred())

				prs, err := gitClient.ListPullRequests(childRepository)
				Expect(err).ShouldNot(HaveOccurred())

				prno := -1
				for _, pr := range prs {
					if pr.SourceBranch == ChildComponentDef.pacBranchName {
						prno = pr.Number
					}
				}
				Expect(prno).ShouldNot(Equal(-1))

				// GitLab merge fails if the pipeline run has not finished
				Eventually(func() error {
					_, err = gitClient.MergePullRequest(childRepository, prno)
					return err
				}, 10*time.Minute, time.Minute).ShouldNot(HaveOccurred(), fmt.Sprintf("unable to merge PR #%d in %s", prno, ChildComponentDef.repoName))

			})
			// This actually happens immediately, but we only need the PR number now
			It(fmt.Sprintf("should lead to a PaC PR creation for parent component %s", ParentComponentDef.componentName), func() {
				timeout = time.Second * 300
				interval := time.Second * 1

				Eventually(func() bool {
					prs, err := gitClient.ListPullRequests(parentRepository)
					Expect(err).ShouldNot(HaveOccurred())

					for _, pr := range prs {
						if pr.SourceBranch == ParentComponentDef.pacBranchName {
							prNumber = pr.Number
							return true
						}
					}
					return false
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("timed out when waiting for PaC PR (branch name '%s') to be created in %s repository", ParentComponentDef.pacBranchName, ParentComponentDef.repoName))
			})
			It(fmt.Sprintf("Merging the PaC PR should be successful for parent component %s", ParentComponentDef.componentName), func() {
				Eventually(func() error {
					mergeResult, err = gitClient.MergePullRequest(parentRepository, prNumber)
					return err
				}, time.Minute).Should(BeNil(), fmt.Sprintf("error when merging PaC pull request #%d in repo %s", prNumber, ParentComponentDef.repoName))

				mergeResultSha = mergeResult.MergeCommitSHA
				GinkgoWriter.Printf("merged result sha: %s for PR #%d\n", mergeResultSha, prNumber)
			})
			// Now the PR is merged this will kick off another build. The result of this build is what we want to update in dockerfile we created
			It(fmt.Sprintf("PR merge triggers PAC PipelineRun for parent component %s", ParentComponentDef.componentName), func() {
				timeout = time.Minute * 5

				Eventually(func() error {
					pipelineRun, err := f.AsKubeAdmin.HasController.GetComponentPipelineRun(ParentComponentDef.componentName, applicationName, testNamespace, mergeResultSha)
					if err != nil {
						GinkgoWriter.Printf("Push PipelineRun has not been created yet for the component %s/%s\n", testNamespace, ParentComponentDef.componentName)
						return err
					}
					if !pipelineRun.HasStarted() {
						return fmt.Errorf("push pipelinerun %s/%s hasn't started yet", pipelineRun.GetNamespace(), pipelineRun.GetName())
					}
					return nil
				}, timeout, constants.PipelineRunPollingInterval).Should(Succeed(), fmt.Sprintf("timed out when waiting for the PipelineRun to start for the component %s/%s", testNamespace, ParentComponentDef.componentName))
			})
			// Wait for this PR to be done and store the digest, we will need it to verify that the nudge was correct
			It(fmt.Sprintf("PAC PipelineRun for parent component %s is successful", ParentComponentDef.componentName), func() {
				pr := &pipeline.PipelineRun{}
				Expect(f.AsKubeAdmin.HasController.WaitForComponentPipelineToBeFinished(ParentComponentDef.component, mergeResultSha, f.AsKubeAdmin.TektonController, &has.RetryOptions{Always: true, Retries: 2}, pr)).To(Succeed())

				for _, result := range pr.Status.PipelineRunStatusFields.Results {
					if result.Name == "IMAGE_DIGEST" {
						parentPostPacMergeDigest = result.Value.StringVal
					}
				}
				Expect(parentPostPacMergeDigest).ShouldNot(BeEmpty())
			})
			It(fmt.Sprintf("should lead to a nudge PR creation for child component %s", ChildComponentDef.componentName), func() {
				timeout = time.Minute * 20
				interval := time.Second * 1

				Eventually(func() bool {
					prs, err := gitClient.ListPullRequests(componentDependenciesChildRepository)
					Expect(err).ShouldNot(HaveOccurred())

					for _, pr := range prs {
						if strings.Contains(pr.SourceBranch, ParentComponentDef.componentName) {
							prNumber = pr.Number
							return true
						}
					}
					return false
				}, timeout, interval).Should(BeTrue(), fmt.Sprintf("timed out when waiting for component nudge PR to be created in %s repository", componentDependenciesChildRepoName))
			})
			It(fmt.Sprintf("merging the PR should be successful for child component %s", ChildComponentDef.componentName), func() {
				Eventually(func() error {
					mergeResult, err = gitClient.MergePullRequest(componentDependenciesChildRepository, prNumber)
					return err
				}, time.Minute).Should(BeNil(), fmt.Sprintf("error when merging nudge pull request #%d in repo %s", prNumber, componentDependenciesChildRepoName))

				mergeResultSha = mergeResult.MergeCommitSHA
				GinkgoWriter.Printf("merged result sha: %s for PR #%d\n", mergeResultSha, prNumber)

			})
			// Now the nudge has been merged we verify the dockerfile is what we expected
			It("Verify the nudge updated the contents", func() {

				GinkgoWriter.Printf("Verifying Dockerfile.tmp updated to sha %s", parentPostPacMergeDigest)
				file, err := gitClient.GetFile(childRepository, "Dockerfile.tmp", ChildComponentDef.componentBranch)
				Expect(err).ShouldNot(HaveOccurred())
				GinkgoWriter.Printf("content: %s\n", file.Content)
				Expect(file.Content).Should(Equal("FROM quay.io/" + quayOrg + "/" + imageRepoName + "@" + parentPostPacMergeDigest + "\nRUN echo hello\n"))

				file, err = gitClient.GetFile(childRepository, "manifest.yaml", ChildComponentDef.componentBranch)
				Expect(err).ShouldNot(HaveOccurred())
				Expect(file.Content).Should(Equal("image: " + distributionRepository + "@" + parentPostPacMergeDigest))

			})
		})
	},
		Entry("github", git.GitHubProvider, "gh"),
		Entry("gitlab", git.GitLabProvider, "gl"),
	)
})

func setupGitProvider(f *framework.Framework, gitProvider git.GitProvider) (git.Client, string, string) {
	switch gitProvider {
	case git.GitHubProvider:
		gitClient := git.NewGitHubClient(f.AsKubeAdmin.CommonController.Github)
		return gitClient, helloWorldComponentGitHubURL, helloWorldComponentGitSourceRepoName
	case git.GitLabProvider:
		gitClient := git.NewGitlabClient(f.AsKubeAdmin.CommonController.Gitlab)

		gitlabToken := utils.GetEnv(constants.GITLAB_BOT_TOKEN_ENV, "")
		Expect(gitlabToken).ShouldNot(BeEmpty())

		secretAnnotations := map[string]string{}

		err := build.CreateGitlabBuildSecret(f, "pipelines-as-code-secret", secretAnnotations, gitlabToken)
		Expect(err).ShouldNot(HaveOccurred())

		return gitClient, helloWorldComponentGitLabURL, helloWorldComponentGitLabProjectID
	}
	return nil, "", ""
}

func createBuildSecret(f *framework.Framework, secretName string, annotations map[string]string, token string) error {
	buildSecret := v1.Secret{}
	buildSecret.Name = secretName
	buildSecret.Labels = map[string]string{
		"appstudio.redhat.com/credentials": "scm",
		"appstudio.redhat.com/scm.host":    "github.com",
	}
	if annotations != nil {
		buildSecret.Annotations = annotations
	}
	buildSecret.Type = "kubernetes.io/basic-auth"
	buildSecret.StringData = map[string]string{
		"password": token,
	}
	_, err := f.AsKubeAdmin.CommonController.CreateSecret(f.UserNamespace, &buildSecret)
	if err != nil {
		return fmt.Errorf("error creating build secret: %v", err)
	}
	return nil
}
