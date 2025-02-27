/*
Copyright 2020 The Knative Authors

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

package certificate

import (
	"context"
	"errors"
	"fmt"
	"hash/adler32"
	"testing"
	"time"

	acmev1 "github.com/cert-manager/cert-manager/pkg/apis/acme/v1"
	cmv1 "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgotesting "k8s.io/client-go/testing"

	fakecertmanagerclient "knative.dev/net-certmanager/pkg/client/certmanager/injection/client/fake"
	"knative.dev/net-certmanager/pkg/reconciler/certificate/config"
	"knative.dev/net-certmanager/pkg/reconciler/certificate/resources"
	netapi "knative.dev/networking/pkg/apis/networking"
	"knative.dev/networking/pkg/apis/networking/v1alpha1"
	networkingclient "knative.dev/networking/pkg/client/injection/client/fake"
	certreconciler "knative.dev/networking/pkg/client/injection/reconciler/networking/v1alpha1/certificate"
	netcfg "knative.dev/networking/pkg/config"
	"knative.dev/pkg/apis"
	duckv1 "knative.dev/pkg/apis/duck/v1"
	"knative.dev/pkg/configmap"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
	pkgreconciler "knative.dev/pkg/reconciler"
	"knative.dev/pkg/system"

	. "knative.dev/net-certmanager/pkg/reconciler/testing"
	. "knative.dev/pkg/reconciler/testing"

	_ "knative.dev/net-certmanager/pkg/client/certmanager/injection/informers/acme/v1/challenge/fake"
	_ "knative.dev/net-certmanager/pkg/client/certmanager/injection/informers/certmanager/v1/certificate/fake"
	_ "knative.dev/net-certmanager/pkg/client/certmanager/injection/informers/certmanager/v1/clusterissuer/fake"
	_ "knative.dev/networking/pkg/client/injection/informers/networking/v1alpha1/certificate/fake"
	_ "knative.dev/pkg/client/injection/kube/informers/core/v1/service/fake"
)

const generation = 23132

var (
	correctDNSNames   = []string{"correct-dns1.example.com", "correct-dns2.example.com"}
	shortenedDNSNames = []string{"k.example.com", "reallyreallyreallyreallyreallyreallylongname.namespace.example.com"}
	incorrectDNSNames = []string{"incorrect-dns.example.com"}
	exampleDomain     = "example.com"
	notAfter          = &metav1.Time{
		Time: time.Unix(123, 456),
	}
	clusterInternalIssuer = &cmv1.ClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "knative-internal-encryption-issuer",
		},
		Spec: cmv1.IssuerSpec{},
	}
	nonHTTP01Issuer = &cmv1.ClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "Letsencrypt-issuer",
		},
		Spec: cmv1.IssuerSpec{},
	}
	http01Issuer = &cmv1.ClusterIssuer{
		ObjectMeta: metav1.ObjectMeta{
			Name: "Letsencrypt-issuer",
		},
		Spec: cmv1.IssuerSpec{
			IssuerConfig: cmv1.IssuerConfig{
				ACME: &acmev1.ACMEIssuer{
					Solvers: []acmev1.ACMEChallengeSolver{{
						HTTP01: &acmev1.ACMEChallengeSolverHTTP01{},
					}},
				},
			},
		},
	}

	externalCert, _                  = resources.MakeCertManagerCertificate(certmanagerConfig(), knCert("knCert", "foo"))
	internalCert, _                  = resources.MakeCertManagerCertificate(certmanagerConfig(), withClusterLocalVisibility(knCert("knCert", "foo")))
	externalCertShortenedDNSNames, _ = resources.MakeCertManagerCertificate(certmanagerConfig(), knCertShortenedDNSNames("knCert", "foo"))
)

func TestNewController(t *testing.T) {
	ctx, _ := SetupFakeContext(t)

	configMapWatcher := configmap.NewStaticWatcher(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.CertManagerConfigName,
			Namespace: system.Namespace(),
		},
		Data: map[string]string{
			"issuerRef":                "kind: ClusterIssuer\nname: letsencrypt-issuer",
			"clusterInternalIssuerRef": "kind: ClusterIssuer\nname: knative-internal-encryption-issuer",
		},
	})

	c := NewController(ctx, configMapWatcher)
	if c == nil {
		t.Fatal("Expected NewController to return a non-nil value")
	}
}

// This is heavily based on the way the OpenShift Ingress controller tests its reconciliation method.
func TestReconcile(t *testing.T) {
	retryAttempted := false
	table := TableTest{{
		Name: "bad workqueue key",
		Key:  "too/many/parts",
	}, {
		Name: "key not found",
		Key:  "foo/not-found",
	}, {
		Name: "create CM certificate matching Knative Certificate, with retry",
		Objects: []runtime.Object{
			knCert("knCert", "foo"),
			nonHTTP01Issuer,
		},
		WithReactors: []clientgotesting.ReactionFunc{
			func(action clientgotesting.Action) (handled bool, ret runtime.Object, err error) {
				if retryAttempted || !action.Matches("update", "certificates") || action.GetSubresource() != "status" {
					return false, nil, nil
				}
				retryAttempted = true
				return true, nil, apierrs.NewConflict(v1alpha1.Resource("foo"), "bar", errors.New("foo"))
			},
		},
		WantCreates: []runtime.Object{
			externalCert,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Severity: apis.ConditionSeverityError,
							Reason:   noCMConditionReason,
							Message:  noCMConditionMessage,
						}},
					},
				}),
		}, {
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Severity: apis.ConditionSeverityError,
							Reason:   noCMConditionReason,
							Message:  noCMConditionMessage,
						}},
					},
				}),
		}},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "Created", "Created Cert-Manager Certificate %s/%s", "foo", "knCert"),
		},
		Key: "foo/knCert",
	}, {
		Name: "reconcile CM certificate to match desired one",
		Objects: []runtime.Object{
			knCert("knCert", "foo"),
			cmCert("knCert", "foo", incorrectDNSNames),
			nonHTTP01Issuer,
		},
		WantUpdates: []clientgotesting.UpdateActionImpl{{
			Object: cmCert("knCert", "foo", correctDNSNames),
		}},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Severity: apis.ConditionSeverityError,
							Reason:   noCMConditionReason,
							Message:  noCMConditionMessage,
						}},
					},
				}),
		}},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "Updated", "Updated Spec for Cert-Manager Certificate %s/%s", "foo", "knCert"),
		},
		Key: "foo/knCert",
	}, {
		Name: "observed generation is still updated when error is encountered, and ready status is unknown",
		Objects: []runtime.Object{
			knCertWithStatusAndGeneration("knCert", "foo",
				&v1alpha1.CertificateStatus{
					Status: duckv1.Status{
						ObservedGeneration: generation + 1,
						Conditions: duckv1.Conditions{{
							Type:   v1alpha1.CertificateConditionReady,
							Status: corev1.ConditionTrue,
						}},
					},
				}, generation+1),
			cmCert("knCert", "foo", incorrectDNSNames),
			nonHTTP01Issuer,
		},
		WantErr: true,
		WithReactors: []clientgotesting.ReactionFunc{
			InduceFailure("update", "certificates"),
		},
		WantUpdates: []clientgotesting.UpdateActionImpl{{
			Object: cmCert("knCert", "foo", correctDNSNames),
		}},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatusAndGeneration("knCert", "foo",
				&v1alpha1.CertificateStatus{
					Status: duckv1.Status{
						ObservedGeneration: generation + 1,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Severity: apis.ConditionSeverityError,
							Reason:   notReconciledReason,
							Message:  notReconciledMessage,
						}},
					},
				}, generation+1),
		}},
		WantEvents: []string{
			Eventf(corev1.EventTypeWarning, "UpdateFailed", "Failed to create Cert-Manager Certificate %s: %v",
				"foo/knCert", "inducing failure for update certificates"),
			Eventf(corev1.EventTypeWarning, "UpdateFailed", "Failed to update status for %q: %v",
				"knCert", "inducing failure for update certificates"),
		},
		Key: "foo/knCert",
	}, {
		Name: "set Knative Certificate ready status with CM Certificate ready status",
		Objects: []runtime.Object{
			knCert("knCert", "foo"),
			cmCertWithStatus("knCert", "foo", correctDNSNames, []cmv1.CertificateCondition{{
				Type:   cmv1.CertificateConditionReady,
				Status: cmmeta.ConditionTrue}}, nil),
			nonHTTP01Issuer,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					NotAfter: notAfter,
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionTrue,
							Severity: apis.ConditionSeverityError,
						}},
					},
				}),
		}},
		Key: "foo/knCert",
	}, {
		Name: "set Knative Certificate unknown status with CM Certificate unknown status",
		Objects: []runtime.Object{
			knCert("knCert", "foo"),
			cmCertWithStatus("knCert", "foo", correctDNSNames, []cmv1.CertificateCondition{{
				Type:   cmv1.CertificateConditionReady,
				Status: cmmeta.ConditionUnknown}}, nil),
			nonHTTP01Issuer,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					NotAfter: notAfter,
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Severity: apis.ConditionSeverityError,
						}},
					},
				}),
		}},
		Key: "foo/knCert",
	}, {
		Name: "set Knative Certificate not ready status with CM Certificate not ready status",
		Objects: []runtime.Object{
			knCert("knCert", "foo"),
			cmCertWithStatus("knCert", "foo", correctDNSNames, []cmv1.CertificateCondition{{
				Type:   cmv1.CertificateConditionReady,
				Status: cmmeta.ConditionFalse}}, nil),
			nonHTTP01Issuer,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					NotAfter: notAfter,
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionFalse,
							Severity: apis.ConditionSeverityError,
						}},
					},
				}),
		}},
		Key: "foo/knCert",
	}, {
		Name: "set Knative Certificate not ready status with details when common name is too long",
		Objects: []runtime.Object{
			knCertDomainTooLong("knCert", "foo", &v1alpha1.CertificateStatus{}, 0),
		},
		WantErr: true,
		WantEvents: []string{
			"Warning InternalError error creating cert-manager certificate: CommonName (reallyreallyreallyreallyreallyreallyreallyreallylong.domainname)(length: 63) too long, prepending short prefix of (k.)(length: 2) will be longer than 64 bytes",
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertDomainTooLong("knCert", "foo",
				&v1alpha1.CertificateStatus{
					Status: duckv1.Status{
						ObservedGeneration: 0,
						Conditions: duckv1.Conditions{
							{
								Type:     v1alpha1.CertificateConditionReady,
								Status:   corev1.ConditionFalse,
								Severity: apis.ConditionSeverityError,
								Reason:   "CommonName Too Long",
								Message:  "error creating cert-manager certificate: CommonName (reallyreallyreallyreallyreallyreallyreallyreallylong.domainname)(length: 63) too long, prepending short prefix of (k.)(length: 2) will be longer than 64 bytes",
							},
						},
					},
				}, 0),
		}},
		Key: "foo/knCert",
	}, {
		Name: "set Knative Certificate renewing status with CM Certificate Renewing status",
		Objects: []runtime.Object{
			knCertWithStatus("knCert", "foo", &v1alpha1.CertificateStatus{
				Status: duckv1.Status{
					ObservedGeneration: generation,
					Conditions: duckv1.Conditions{
						{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionTrue,
							Severity: apis.ConditionSeverityError,
						},
					},
				},
			}),
			cmCertWithStatus("knCert", "foo", correctDNSNames, []cmv1.CertificateCondition{
				{
					Type:   cmv1.CertificateConditionReady,
					Status: cmmeta.ConditionTrue,
				},
				{
					Type:   cmv1.CertificateConditionIssuing,
					Status: cmmeta.ConditionTrue,
					Reason: renewingEvent,
				},
			}, &metav1.Time{
				Time: time.Now(),
			}),
			nonHTTP01Issuer,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					NotAfter: notAfter,
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{
							{
								Type:     v1alpha1.CertificateConditionReady,
								Status:   corev1.ConditionTrue,
								Severity: apis.ConditionSeverityError,
							},
							{
								Type:     renewingEvent,
								Status:   corev1.ConditionTrue,
								Severity: apis.ConditionSeverityError,
							},
						},
					},
				}),
		}},
		Key: "foo/knCert",
	}, {
		Name: "set Knative Certificate ready status after a renew with CM Certificate ready status",
		Objects: []runtime.Object{
			knCertWithStatus("knCert", "foo", &v1alpha1.CertificateStatus{
				Status: duckv1.Status{
					ObservedGeneration: generation,
					Conditions: duckv1.Conditions{
						{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionTrue,
							Severity: apis.ConditionSeverityError,
						},
						{
							Type:     renewingEvent,
							Status:   corev1.ConditionTrue,
							Severity: apis.ConditionSeverityError,
						},
					},
				},
			}),
			cmCertWithStatus("knCert", "foo", correctDNSNames, []cmv1.CertificateCondition{
				{
					Type:   cmv1.CertificateConditionReady,
					Status: cmmeta.ConditionTrue,
				},
			}, &metav1.Time{
				Time: time.Now().Add(5 * time.Minute),
			}),
			nonHTTP01Issuer,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					NotAfter: notAfter,
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{
							{
								Type:     v1alpha1.CertificateConditionReady,
								Status:   corev1.ConditionTrue,
								Severity: apis.ConditionSeverityError,
							},
						},
					},
				}),
		}},
		Key: "foo/knCert",
	}, {
		Name: "reconcile cm certificate fails",
		Key:  "foo/knCert",
		Objects: []runtime.Object{
			knCert("knCert", "foo"),
			nonHTTP01Issuer,
		},
		WantErr: true,
		WithReactors: []clientgotesting.ReactionFunc{
			InduceFailure("create", "certificates"),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeWarning, "CreationFailed", "Failed to create Cert-Manager Certificate knCert/foo: inducing failure for create certificates"),
			Eventf(corev1.EventTypeWarning, "InternalError", "failed to create Cert-Manager Certificate: inducing failure for create certificates"),
		},
		WantCreates: []runtime.Object{
			externalCert,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Reason:   notReconciledReason,
							Severity: apis.ConditionSeverityError,
							Message:  notReconciledMessage,
						}},
					},
				}),
		}},
	}, {
		Name: "create clusterInternalIssuer CM certificate matching Knative Certificate, with retry",
		Key:  "foo/knCert",
		Objects: []runtime.Object{
			withClusterLocalVisibility(knCert("knCert", "foo")),
			clusterInternalIssuer,
		},
		WantErr: true,
		WithReactors: []clientgotesting.ReactionFunc{
			InduceFailure("create", "certificates"),
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeWarning, "CreationFailed", "Failed to create Cert-Manager Certificate knCert/foo: inducing failure for create certificates"),
			Eventf(corev1.EventTypeWarning, "InternalError", "failed to create Cert-Manager Certificate: inducing failure for create certificates"),
		},
		WantCreates: []runtime.Object{
			internalCert,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: withClusterLocalVisibility(knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Reason:   notReconciledReason,
							Severity: apis.ConditionSeverityError,
							Message:  notReconciledMessage,
						}},
					},
				})),
		}},
	}}

	table.Test(t, MakeFactory(func(ctx context.Context, listers *Listers, cmw configmap.Watcher) controller.Reconciler {
		retryAttempted = false
		r := &Reconciler{
			cmCertificateLister: listers.GetCMCertificateLister(),
			cmChallengeLister:   listers.GetCMChallengeLister(),
			cmIssuerLister:      listers.GetCMClusterIssuerLister(),
			svcLister:           listers.GetK8sServiceLister(),
			certManagerClient:   fakecertmanagerclient.Get(ctx),
			tracker:             &NullTracker{},
		}
		return certreconciler.NewReconciler(ctx, logging.FromContext(ctx), networkingclient.Get(ctx),
			listers.GetCertificateLister(), controller.GetEventRecorder(ctx), r,
			netcfg.CertManagerCertificateClassName, controller.Options{
				ConfigStore: &testConfigStore{
					config: &config.Config{
						CertManager: certmanagerConfig(),
					},
				},
			})
	}))
}

func TestReconcile_HTTP01Challenges(t *testing.T) {
	table := TableTest{{
		Name:    "fail to set status.HTTP01Challenges",
		Key:     "foo/knCert",
		WantErr: true,
		Objects: []runtime.Object{
			knCert("knCert", "foo"),
			http01Issuer,
		},
		WantCreates: []runtime.Object{
			externalCert,
		},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "Created", "Created Cert-Manager Certificate %s/%s", "foo", "knCert"),
			Eventf(corev1.EventTypeWarning, "InternalError", "no challenge solver service for domain %s; selector=acme.cert-manager.io/http-domain=1930889501", correctDNSNames[0]),
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Reason:   notReconciledReason,
							Severity: apis.ConditionSeverityError,
							Message:  notReconciledMessage,
						}},
					},
				}),
		}},
	}, {
		Name: "set Status.HTTP01Challenges on Knative certificate",
		Key:  "foo/knCert",
		Objects: []runtime.Object{
			cmSolverService(correctDNSNames[0], "foo"),
			cmSolverService(correctDNSNames[1], "foo"),
			cmChallenge(correctDNSNames[0], "foo"),
			cmChallenge(correctDNSNames[1], "foo"),
			cmCert("knCert", "foo", correctDNSNames),
			knCert("knCert", "foo"),
			http01Issuer,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					HTTP01Challenges: []v1alpha1.HTTP01Challenge{{
						URL: &apis.URL{
							Scheme: "http",
							Host:   correctDNSNames[0],
							Path:   "/.well-known/acme-challenge/cm-challenge-token",
						},
						ServiceName:      "cm-solver-" + correctDNSNames[0],
						ServiceNamespace: "foo",
					}, {
						URL: &apis.URL{
							Scheme: "http",
							Host:   correctDNSNames[1],
							Path:   "/.well-known/acme-challenge/cm-challenge-token",
						},
						ServiceName:      "cm-solver-" + correctDNSNames[1],
						ServiceNamespace: "foo",
					}},
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Severity: apis.ConditionSeverityError,
							Reason:   noCMConditionReason,
							Message:  noCMConditionMessage,
						}},
					},
				}),
		}},
	}, {
		Name: "set Status.HTTP01Challenges on Knative certificate when status failed with InProgress",
		Key:  "foo/knCert",
		Objects: []runtime.Object{
			cmSolverService(correctDNSNames[0], "foo"),
			cmSolverService(correctDNSNames[1], "foo"),
			cmChallenge(correctDNSNames[0], "foo"),
			cmChallenge(correctDNSNames[1], "foo"),
			cmCertWithStatus("knCert", "foo", correctDNSNames, []cmv1.CertificateCondition{{
				Type:   cmv1.CertificateConditionReady,
				Status: cmmeta.ConditionFalse,
				Reason: "InProgress"}}, nil),
			knCert("knCert", "foo"),
			http01Issuer,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{{
			Object: knCertWithStatus("knCert", "foo",
				&v1alpha1.CertificateStatus{
					NotAfter: notAfter,
					HTTP01Challenges: []v1alpha1.HTTP01Challenge{{
						URL: &apis.URL{
							Scheme: "http",
							Host:   correctDNSNames[0],
							Path:   "/.well-known/acme-challenge/cm-challenge-token",
						},
						ServiceName:      "cm-solver-" + correctDNSNames[0],
						ServiceNamespace: "foo",
					}, {
						URL: &apis.URL{
							Scheme: "http",
							Host:   correctDNSNames[1],
							Path:   "/.well-known/acme-challenge/cm-challenge-token",
						},
						ServiceName:      "cm-solver-" + correctDNSNames[1],
						ServiceNamespace: "foo",
					}},
					Status: duckv1.Status{
						ObservedGeneration: generation,
						Conditions: duckv1.Conditions{{
							Type:     v1alpha1.CertificateConditionReady,
							Status:   corev1.ConditionUnknown,
							Severity: apis.ConditionSeverityError,
							Reason:   "InProgress",
						}},
					},
				}),
		}},
	}, {
		//It is possible for a challenge to not be created for a k.{{Domain}} dnsname, since it may have already been created in a previous Kservice
		Name: "set Status.HTTP01Challenges on Knative certificate when shortened domain with prefix (k.) is reused",
		Key:  "foo/knCert",
		Objects: []runtime.Object{
			cmSolverService(shortenedDNSNames[1], "foo"),
			cmChallenge(shortenedDNSNames[1], "foo"),
			cmCert("knCert", "foo", shortenedDNSNames),
			knCertShortenedDNSNames("knCert", "foo"),
			http01Issuer,
		},
		WantStatusUpdates: []clientgotesting.UpdateActionImpl{
			{
				Object: knCertShortenedDNSNamesWithStatus("knCert", "foo",
					&v1alpha1.CertificateStatus{
						HTTP01Challenges: []v1alpha1.HTTP01Challenge{{
							URL: &apis.URL{
								Scheme: "http",
								Host:   shortenedDNSNames[1],
								Path:   "/.well-known/acme-challenge/cm-challenge-token",
							},
							ServiceName:      "cm-solver-" + shortenedDNSNames[1],
							ServiceNamespace: "foo",
						}},
						Status: duckv1.Status{
							ObservedGeneration: generation,
							Conditions: duckv1.Conditions{{
								Type:     v1alpha1.CertificateConditionReady,
								Status:   corev1.ConditionUnknown,
								Severity: apis.ConditionSeverityError,
								Reason:   noCMConditionReason,
								Message:  noCMConditionMessage,
							}},
						},
					}),
			},
		},
		WantUpdates: []clientgotesting.UpdateActionImpl{{
			Object: externalCertShortenedDNSNames,
		}},
		WantEvents: []string{
			Eventf(corev1.EventTypeNormal, "Updated", "Updated Spec for Cert-Manager Certificate %s/%s", "foo", "knCert"),
		},
	}}

	table.Test(t, MakeFactory(func(ctx context.Context, listers *Listers, cmw configmap.Watcher) controller.Reconciler {
		r := &Reconciler{
			cmCertificateLister: listers.GetCMCertificateLister(),
			cmChallengeLister:   listers.GetCMChallengeLister(),
			cmIssuerLister:      listers.GetCMClusterIssuerLister(),
			svcLister:           listers.GetK8sServiceLister(),
			certManagerClient:   fakecertmanagerclient.Get(ctx),
			tracker:             &NullTracker{},
		}
		return certreconciler.NewReconciler(ctx, logging.FromContext(ctx), networkingclient.Get(ctx),
			listers.GetCertificateLister(), controller.GetEventRecorder(ctx), r,
			netcfg.CertManagerCertificateClassName, controller.Options{
				ConfigStore: &testConfigStore{
					config: &config.Config{
						CertManager: certmanagerConfig(),
					},
				},
			})
	}))
}

type testConfigStore struct {
	config *config.Config
}

func (t *testConfigStore) ToContext(ctx context.Context) context.Context {
	return config.ToContext(ctx, t.config)
}

var _ pkgreconciler.ConfigStore = (*testConfigStore)(nil)

func certmanagerConfig() *config.CertManagerConfig {
	return &config.CertManagerConfig{
		IssuerRef: &cmmeta.ObjectReference{
			Kind: "ClusterIssuer",
			Name: "Letsencrypt-issuer",
		},
		ClusterInternalIssuerRef: &cmmeta.ObjectReference{
			Kind: "ClusterIssuer",
			Name: "knative-internal-encryption-issuer",
		},
	}
}

func knCert(name, namespace string) *v1alpha1.Certificate {
	return knCertWithStatus(name, namespace, &v1alpha1.CertificateStatus{})
}

func knCertShortenedDNSNames(name, namespace string) *v1alpha1.Certificate {
	cert := knCertWithStatus(name, namespace, &v1alpha1.CertificateStatus{})
	cert.Spec.DNSNames = shortenedDNSNames
	return cert
}

func knCertDomainTooLong(name, namespace string, status *v1alpha1.CertificateStatus, gen int) *v1alpha1.Certificate {
	return &v1alpha1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: int64(gen),
			Annotations: map[string]string{
				netapi.CertificateClassAnnotationKey: netcfg.CertManagerCertificateClassName,
			},
		},
		Spec: v1alpha1.CertificateSpec{
			DNSNames:   []string{"hello.ns.reallyreallyreallyreallyreallyreallyreallyreallylong.domainname"},
			Domain:     "reallyreallyreallyreallyreallyreallyreallyreallylong.domainname",
			SecretName: "secret0",
		},
		Status: *status,
	}
}

func knCertWithStatus(name, namespace string, status *v1alpha1.CertificateStatus) *v1alpha1.Certificate {
	return knCertWithStatusAndGeneration(name, namespace, status, generation)
}

func knCertShortenedDNSNamesWithStatus(name, namespace string, status *v1alpha1.CertificateStatus) *v1alpha1.Certificate {
	cert := knCertWithStatus(name, namespace, status)
	cert.Spec.DNSNames = shortenedDNSNames
	return cert
}

func knCertWithStatusAndGeneration(name, namespace string, status *v1alpha1.CertificateStatus, gen int) *v1alpha1.Certificate {
	return &v1alpha1.Certificate{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: int64(gen),
			Annotations: map[string]string{
				netapi.CertificateClassAnnotationKey: netcfg.CertManagerCertificateClassName,
			},
		},
		Spec: v1alpha1.CertificateSpec{
			DNSNames:   correctDNSNames,
			Domain:     exampleDomain,
			SecretName: "secret0",
		},
		Status: *status,
	}
}

func withClusterLocalVisibility(certificate *v1alpha1.Certificate) *v1alpha1.Certificate {
	if certificate.ObjectMeta.Labels == nil {
		certificate.ObjectMeta.Labels = map[string]string{}
	}
	certificate.ObjectMeta.Labels[netapi.VisibilityLabelKey] = resources.VisibilityClusterLocal
	return certificate
}

func cmCert(name, namespace string, dnsNames []string) *cmv1.Certificate {
	cert, _ := resources.MakeCertManagerCertificate(certmanagerConfig(), knCert(name, namespace))
	cert.Spec.DNSNames = dnsNames
	return cert
}

func cmCertWithStatus(name, namespace string, dnsNames []string, conditions []cmv1.CertificateCondition, renewalTime *metav1.Time) *cmv1.Certificate {
	cert := cmCert(name, namespace, dnsNames)
	cert.Status.Conditions = conditions
	cert.Status.NotAfter = notAfter
	cert.Status.RenewalTime = renewalTime
	return cert
}

func cmChallenge(hostname, namespace string) *acmev1.Challenge {
	return &acmev1.Challenge{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "challenge-" + hostname,
			Namespace: namespace,
		},
		Spec: acmev1.ChallengeSpec{
			Type:    "http01",
			DNSName: hostname,
			Token:   "cm-challenge-token",
		},
	}
}

func cmSolverService(hostname, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			OwnerReferences: []metav1.OwnerReference{{
				Name: "challenge-" + hostname,
			}},
			Name:      "cm-solver-" + hostname,
			Namespace: namespace,
			Labels: map[string]string{
				httpDomainLabel: fmt.Sprintf("%d", adler32.Checksum([]byte(hostname))),
			},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Port:     8090,
				Protocol: "tcp",
			}},
		},
	}

}
