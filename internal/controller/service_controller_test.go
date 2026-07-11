package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"
	"time"
	"unsafe"

	"github.com/go-logr/logr"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/auth/basic"
	coreconfig "github.com/huaweicloud/huaweicloud-sdk-go-v3/core/config"
	"github.com/huaweicloud/huaweicloud-sdk-go-v3/core/region"
	elb "github.com/huaweicloud/huaweicloud-sdk-go-v3/services/elb/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/weimantian/huawei-elb-controller/internal/huaweicloud"
)

func newTestDetector(vpcID, subnetID string, azs []string) *huaweicloud.NetworkDetector {
d := huaweicloud.NewNetworkDetector(nil)
v := reflect.ValueOf(d).Elem()
f := v.FieldByName("detected")
f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
f.Set(reflect.ValueOf(&huaweicloud.DetectedParams{
VPCID:    vpcID,
SubnetID: subnetID,
AZs:      azs,
}))
t := v.FieldByName("detectedAt")
t = reflect.NewAt(t.Type(), unsafe.Pointer(t.UnsafeAddr())).Elem()
t.Set(reflect.ValueOf(time.Now()))
return d
}

type mockRoundTripper struct {
	serverURL string
}

func (t *mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(t.serverURL)
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}

func newMockELBClient(handler http.Handler) (*elb.ElbClient, *httptest.Server) {
	server := httptest.NewServer(handler)

	transport := &mockRoundTripper{serverURL: server.URL}
	auth := basic.NewCredentialsBuilder().
		WithAk("test-ak").
		WithSk("test-sk").
		WithProjectId("test-project-id").
		Build()

	reg := region.NewRegion("cn-north-4", "https://elb.cn-north-4.myhuaweicloud.com")

	httpConfig := coreconfig.DefaultHttpConfig().
		WithHttpRoundTripper(transport).
		WithTimeout(10 * time.Second)

	hcClient, err := elb.ElbClientBuilder().
		WithCredential(auth).
		WithRegion(reg).
		WithHttpConfig(httpConfig).
		SafeBuild()
	if err != nil {
		server.Close()
		panic("failed to build mock ELB client: " + err.Error())
	}

	return elb.NewElbClient(hcClient), server
}

func makeTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	return scheme
}

func makeTestService(name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "percona-xtradb-cluster-operator",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}
}

func TestServiceReconciler_CreatePath_InjectsAutocreate(t *testing.T) {
	svc := makeTestService("my-svc")
	detector := newTestDetector("vpc-test", "subnet-test", []string{"az1", "az2"})

	r := &ServiceReconciler{
		Client:          fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
		NetworkDetector: detector,
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	result, err := r.reconcileCreate(ctx, logr.Discard(), svc)
	if err != nil {
		t.Fatalf("reconcileCreate returned error: %v", err)
	}
	if result.RequeueAfter != serviceRequeue {
		t.Errorf("expected requeue after %v, got %v", serviceRequeue, result.RequeueAfter)
	}

	autocreate := svc.Annotations[huaweicloud.CCEAutocreateAnnotation]
	if autocreate == "" {
		t.Fatal("expected autocreate annotation to be set")
	}

	var config huaweicloud.AutocreateConfig
	if err := json.Unmarshal([]byte(autocreate), &config); err != nil {
		t.Fatalf("autocreate JSON is invalid: %v", err)
	}
	if config.VipSubnetCidrID != "subnet-test" {
		t.Errorf("expected VipSubnetCidrID=subnet-test, got %s", config.VipSubnetCidrID)
	}
	if config.Type != "public" {
		t.Errorf("expected type=public, got %s", config.Type)
	}
	if config.BandwidthSize != int32(huaweicloud.DefaultBandwidthSize) {
		t.Errorf("expected BandwidthSize=%d, got %d", huaweicloud.DefaultBandwidthSize, config.BandwidthSize)
	}
	if len(config.AvailableZone) != 2 || config.AvailableZone[0] != "az1" || config.AvailableZone[1] != "az2" {
		t.Errorf("expected AvailableZone=[az1 az2], got %v", config.AvailableZone)
	}

	if svc.Annotations[elbClassAnnotation] != "union" {
		t.Errorf("expected elb.class=union, got %s", svc.Annotations[elbClassAnnotation])
	}
	if svc.Annotations[reclaimPolicyAnnotation] != "alwaysDelete" {
		t.Errorf("expected reclaim-policy=alwaysDelete, got %s", svc.Annotations[reclaimPolicyAnnotation])
	}
	if svc.Annotations[aclStatusAnnotation] != "off" {
		t.Errorf("expected acl-status=off, got %s", svc.Annotations[aclStatusAnnotation])
	}

	lastKnownJSON := svc.Annotations[lastKnownParamsAnnotation]
	if lastKnownJSON == "" {
		t.Error("expected last-known-params annotation to be set")
	} else {
		var lastKnown map[string]string
		if err := json.Unmarshal([]byte(lastKnownJSON), &lastKnown); err == nil {
			if lastKnown[sourceRangesKey] == "" {
				t.Error("expected source-ranges key in last-known params")
			}
		}
	}
}

func TestServiceReconciler_CreatePath_WithLBCParams(t *testing.T) {
	svc := makeTestService("my-svc")
	svc.Annotations = map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "50",
		huaweicloud.LBCNameAnnotation:          "custom-name",
	}
	detector := newTestDetector("vpc-x", "subnet-y", []string{"az-a"})

	r := &ServiceReconciler{
		Client:          fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
		NetworkDetector: detector,
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	_, err := r.reconcileCreate(ctx, logr.Discard(), svc)
	if err != nil {
		t.Fatalf("reconcileCreate returned error: %v", err)
	}

	autocreate := svc.Annotations[huaweicloud.CCEAutocreateAnnotation]
	if autocreate == "" {
		t.Fatal("expected autocreate annotation to be set")
	}

	var config huaweicloud.AutocreateConfig
	if err := json.Unmarshal([]byte(autocreate), &config); err != nil {
		t.Fatalf("autocreate JSON is invalid: %v", err)
	}
	if config.BandwidthSize != 50 {
		t.Errorf("expected BandwidthSize=50 from LBC params, got %d", config.BandwidthSize)
	}
	if config.Name != "custom-name" {
		t.Errorf("expected Name=custom-name from LBC params, got %s", config.Name)
	}
}

func TestServiceReconciler_CreatePath_InternalELB(t *testing.T) {
	svc := makeTestService("internal-svc")
	svc.Annotations = map[string]string{
		huaweicloud.LBCPublicAnnotation: "false",
	}
	detector := newTestDetector("vpc-x", "subnet-y", []string{"az-a"})

	r := &ServiceReconciler{
		Client:          fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
		NetworkDetector: detector,
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	_, err := r.reconcileCreate(ctx, logr.Discard(), svc)
	if err != nil {
		t.Fatalf("reconcileCreate returned error: %v", err)
	}

	var config huaweicloud.AutocreateConfig
	if err := json.Unmarshal([]byte(svc.Annotations[huaweicloud.CCEAutocreateAnnotation]), &config); err != nil {
		t.Fatalf("autocreate JSON is invalid: %v", err)
	}
	if config.Type != "" {
		t.Errorf("expected type empty for internal ELB, got %s", config.Type)
	}
	if config.BandwidthSize != 0 {
		t.Errorf("expected BandwidthSize=0 for internal ELB, got %d", config.BandwidthSize)
	}
}

func TestServiceReconciler_CreatePath_ACLWithSourceRanges(t *testing.T) {
	ipGroupHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ipgroup": {"id": "ipgroup-mock-id"}}`))
	})
	mockClient, server := newMockELBClient(ipGroupHandler)
	defer server.Close()

	svc := makeTestService("acl-svc")
	svc.Spec.LoadBalancerSourceRanges = []string{"10.0.0.0/8", "192.168.0.0/16"}
	detector := newTestDetector("vpc-test", "subnet-test", []string{"az1"})

	r := &ServiceReconciler{
		Client:          fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
		NetworkDetector: detector,
		ELBClient:       mockClient,
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	_, err := r.reconcileCreate(ctx, logr.Discard(), svc)
	if err != nil {
		t.Fatalf("reconcileCreate returned error: %v", err)
	}

	if svc.Annotations[aclStatusAnnotation] != "on" {
		t.Errorf("expected acl-status=on, got %s", svc.Annotations[aclStatusAnnotation])
	}
	if svc.Annotations[aclTypeAnnotation] != "white" {
		t.Errorf("expected acl-type=white, got %s", svc.Annotations[aclTypeAnnotation])
	}
	if svc.Annotations[aclIDAnnotation] != "ipgroup-mock-id" {
		t.Errorf("expected acl-id=ipgroup-mock-id, got %s", svc.Annotations[aclIDAnnotation])
	}
}

func TestServiceReconciler_Skips_NonLoadBalancer(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "clusterip-svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
		},
	}

	r := &ServiceReconciler{
		Client: fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "clusterip-svc", Namespace: "default"}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result for non-LoadBalancer, got %v", result)
	}
}

func TestServiceReconciler_Skips_WithELBID(t *testing.T) {
	svc := makeTestService("with-elb-id")
	svc.Annotations = map[string]string{
		"kubernetes.io/elb.id": "elb-existing-id",
	}

	r := &ServiceReconciler{
		Client: fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "with-elb-id", Namespace: "default"}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result when ELB ID exists, got %v", result)
	}
	if svc.Annotations[huaweicloud.CCEAutocreateAnnotation] != "" {
		t.Error("expected no autocreate annotation when ELB ID exists")
	}
}

func TestServiceReconciler_Skips_NonOpenEverest(t *testing.T) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "non-everest-svc",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}

	r := &ServiceReconciler{
		Client: fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "non-everest-svc", Namespace: "default"}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result for non-OpenEverest service, got %v", result)
	}
}

func TestServiceReconciler_Skips_ForeignCloudAnnotations(t *testing.T) {
	svc := makeTestService("gke-svc")
	svc.Annotations = map[string]string{
		"networking.gke.io/load-balancer-type": "Internal",
	}

	r := &ServiceReconciler{
		Client: fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "gke-svc", Namespace: "default"}}

	result, err := r.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if !result.IsZero() {
		t.Errorf("expected zero result for foreign cloud service, got %v", result)
	}
}

func TestServiceReconciler_UpdatePath_ParamsChangedWithMock(t *testing.T) {
	listHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"loadbalancers":[]}`))
	})
	mockClient, server := newMockELBClient(listHandler)
	defer server.Close()

	svc := makeTestService("update-svc")
	svc.Annotations = map[string]string{
		huaweicloud.CCEAutocreateAnnotation:      `{"type":"public"}`,
		lastKnownParamsAnnotation:                `{"huawei-elb.io/bandwidth-size":"10","source-ranges":"[]"}`,
		huaweicloud.LBCBandwidthSizeAnnotation:   "20",
		aclStatusAnnotation:                      "off",
	}

	r := &ServiceReconciler{
		Client:    fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
		ELBClient: mockClient,
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	result, err := r.reconcileUpdate(ctx, logr.Discard(), svc)
	if err != nil {
		t.Fatalf("reconcileUpdate returned error: %v", err)
	}
	if result.RequeueAfter != serviceRequeue {
		t.Errorf("expected requeue after %v, got %v", serviceRequeue, result.RequeueAfter)
	}
}

func TestServiceReconciler_UpdatePath_WithELBID(t *testing.T) {
	svc := makeTestService("elbid-update-svc")
	svc.Annotations = map[string]string{
		huaweicloud.CCEAutocreateAnnotation:    `{"type":"public"}`,
		huaweicloud.AnnotationELBID:            "elb-123",
		lastKnownParamsAnnotation:              `{"huawei-elb.io/bandwidth-size":"10","source-ranges":"[]"}`,
		huaweicloud.LBCBandwidthSizeAnnotation: "10",
		aclStatusAnnotation:                    "off",
	}

	r := &ServiceReconciler{
		Client: fake.NewClientBuilder().WithScheme(makeTestScheme()).WithObjects(svc).Build(),
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	result, err := r.reconcileUpdate(ctx, logr.Discard(), svc)
	if err != nil {
		t.Fatalf("reconcileUpdate returned error: %v", err)
	}
	if result.RequeueAfter != serviceRequeue {
		t.Errorf("expected requeue after %v, got %v", serviceRequeue, result.RequeueAfter)
	}
}

func TestAclAnnotationLogic(t *testing.T) {
	svc := makeTestService("acl-test")
	svc.Annotations = map[string]string{
		huaweicloud.CCEAutocreateAnnotation: `{"type":"public"}`,
		lastKnownParamsAnnotation:           `{"huawei-elb.io/bandwidth-size":"10","source-ranges":"[\"10.0.0.0/8\"]"}`,
	}

	lastKnownJSON := svc.Annotations[lastKnownParamsAnnotation]
	var lastKnownParams map[string]string
	_ = json.Unmarshal([]byte(lastKnownJSON), &lastKnownParams)
	t.Logf("lastKnownParams: %v", lastKnownParams)

	lastSourceRangesJSON := lastKnownParams[sourceRangesKey]
	t.Logf("lastSourceRangesJSON: %q", lastSourceRangesJSON)
	var lastSourceRanges []string
	_ = json.Unmarshal([]byte(lastSourceRangesJSON), &lastSourceRanges)

	currentSourceRanges := svc.Spec.LoadBalancerSourceRanges
	t.Logf("lastSourceRanges=%v, currentSourceRanges=%v", lastSourceRanges, currentSourceRanges)
	t.Logf("sourceRangesEqual=%v", sourceRangesEqual(lastSourceRanges, currentSourceRanges))

	aclChanged := !sourceRangesEqual(lastSourceRanges, currentSourceRanges)
	t.Logf("aclChanged=%v, len(current)=%d", aclChanged, len(currentSourceRanges))

	if aclChanged && len(currentSourceRanges) == 0 {
		svc.Annotations[aclStatusAnnotation] = "off"
	}

	if svc.Annotations[aclStatusAnnotation] != "off" {
		t.Errorf("expected off, got %q. aclChanged=%v", svc.Annotations[aclStatusAnnotation], aclChanged)
	}
}

func TestParamsEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        map[string]string
		b        map[string]string
		expected bool
	}{
		{
			name:     "identical maps",
			a:        map[string]string{"key1": "val1", "key2": "val2"},
			b:        map[string]string{"key1": "val1", "key2": "val2"},
			expected: true,
		},
		{
			name:     "empty maps",
			a:        map[string]string{},
			b:        map[string]string{},
			expected: true,
		},
		{
			name:     "nil maps",
			a:        nil,
			b:        nil,
			expected: true,
		},
		{
			name:     "different values",
			a:        map[string]string{"key1": "val1"},
			b:        map[string]string{"key1": "val2"},
			expected: false,
		},
		{
			name:     "different sizes",
			a:        map[string]string{"key1": "val1", "key2": "val2"},
			b:        map[string]string{"key1": "val1"},
			expected: false,
		},
		{
			name:     "a has extra key",
			a:        map[string]string{"key1": "val1", "key2": "val2"},
			b:        map[string]string{"key1": "val1", "key3": "val3"},
			expected: false,
		},
		{
			name:     "bandwidth size changed",
			a:        map[string]string{huaweicloud.LBCBandwidthSizeAnnotation: "20"},
			b:        map[string]string{huaweicloud.LBCBandwidthSizeAnnotation: "10"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := paramsEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("paramsEqual(%v, %v) = %v, want %v", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

func TestSourceRangesEqual(t *testing.T) {
	tests := []struct {
		name     string
		a        []string
		b        []string
		expected bool
	}{
		{
			name:     "identical slices",
			a:        []string{"10.0.0.0/8", "192.168.0.0/16"},
			b:        []string{"10.0.0.0/8", "192.168.0.0/16"},
			expected: true,
		},
		{
			name:     "both empty",
			a:        []string{},
			b:        []string{},
			expected: true,
		},
		{
			name:     "both nil",
			a:        nil,
			b:        nil,
			expected: true,
		},
		{
			name:     "different order",
			a:        []string{"192.168.0.0/16", "10.0.0.0/8"},
			b:        []string{"10.0.0.0/8", "192.168.0.0/16"},
			expected: false,
		},
		{
			name:     "different lengths",
			a:        []string{"10.0.0.0/8"},
			b:        []string{"10.0.0.0/8", "192.168.0.0/16"},
			expected: false,
		},
		{
			name:     "different values",
			a:        []string{"10.0.0.0/8"},
			b:        []string{"172.16.0.0/12"},
			expected: false,
		},
		{
			name:     "nil vs empty",
			a:        nil,
			b:        []string{},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sourceRangesEqual(tt.a, tt.b)
			if result != tt.expected {
				t.Errorf("sourceRangesEqual(%v, %v) = %v, want %v", tt.a, tt.b, result, tt.expected)
			}
		})
	}
}

func TestBuildUpdateOption_BandwidthSizeChanged(t *testing.T) {
	current := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "50",
	}
	lastKnown := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "10",
	}

	opt := buildUpdateOption(current, lastKnown)
	if opt.BandwidthSize != 50 {
		t.Errorf("expected BandwidthSize=50, got %d", opt.BandwidthSize)
	}
	if opt.BandwidthChargeMode != "" {
		t.Errorf("expected no charge mode change, got %s", opt.BandwidthChargeMode)
	}
}

func TestBuildUpdateOption_BandwidthChargeModeChanged(t *testing.T) {
	current := map[string]string{
		huaweicloud.LBCBandwidthChargeModeAnnotation: "bandwidth",
	}
	lastKnown := map[string]string{
		huaweicloud.LBCBandwidthChargeModeAnnotation: "traffic",
	}

	opt := buildUpdateOption(current, lastKnown)
	if opt.BandwidthChargeMode != "bandwidth" {
		t.Errorf("expected BandwidthChargeMode=bandwidth, got %s", opt.BandwidthChargeMode)
	}
	if opt.BandwidthSize != 0 {
		t.Errorf("expected no bandwidth size change, got %d", opt.BandwidthSize)
	}
}

func TestBuildUpdateOption_BothChanged(t *testing.T) {
	current := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation:       "100",
		huaweicloud.LBCBandwidthChargeModeAnnotation: "bandwidth",
	}
	lastKnown := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation:       "10",
		huaweicloud.LBCBandwidthChargeModeAnnotation: "traffic",
	}

	opt := buildUpdateOption(current, lastKnown)
	if opt.BandwidthSize != 100 {
		t.Errorf("expected BandwidthSize=100, got %d", opt.BandwidthSize)
	}
	if opt.BandwidthChargeMode != "bandwidth" {
		t.Errorf("expected BandwidthChargeMode=bandwidth, got %s", opt.BandwidthChargeMode)
	}
}

func TestBuildUpdateOption_NoChanges(t *testing.T) {
	current := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation:       "10",
		huaweicloud.LBCBandwidthChargeModeAnnotation: "traffic",
	}
	lastKnown := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation:       "10",
		huaweicloud.LBCBandwidthChargeModeAnnotation: "traffic",
	}

	opt := buildUpdateOption(current, lastKnown)
	if opt.BandwidthSize != 0 {
		t.Errorf("expected no bandwidth size change, got %d", opt.BandwidthSize)
	}
	if opt.BandwidthChargeMode != "" {
		t.Errorf("expected no charge mode change, got %s", opt.BandwidthChargeMode)
	}
}

func TestBuildUpdateOption_InvalidBandwidthSize(t *testing.T) {
	current := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "invalid",
	}
	lastKnown := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "10",
	}

	opt := buildUpdateOption(current, lastKnown)
	if opt.BandwidthSize != 0 {
		t.Errorf("expected BandwidthSize=0 for invalid input, got %d", opt.BandwidthSize)
	}
}

func TestBuildUpdateOption_ZeroBandwidthSize(t *testing.T) {
	current := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "0",
	}
	lastKnown := map[string]string{
		huaweicloud.LBCBandwidthSizeAnnotation: "10",
	}

	opt := buildUpdateOption(current, lastKnown)
	if opt.BandwidthSize != 0 {
		t.Errorf("expected BandwidthSize=0 for zero input, got %d", opt.BandwidthSize)
	}
}

func TestBuildUpdateOption_MissingKeys(t *testing.T) {
	current := map[string]string{}
	lastKnown := map[string]string{}

	opt := buildUpdateOption(current, lastKnown)
	if opt.BandwidthSize != 0 {
		t.Errorf("expected no bandwidth size change, got %d", opt.BandwidthSize)
	}
	if opt.BandwidthChargeMode != "" {
		t.Errorf("expected no charge mode change, got %s", opt.BandwidthChargeMode)
	}
}

func TestServiceReconciler_CreatePath_DetectorError(t *testing.T) {
	svc := makeTestService("detector-error-svc")
	detector := huaweicloud.NewNetworkDetector(nil)

	r := &ServiceReconciler{
		Client:          fake.NewClientBuilder().WithScheme(makeTestScheme()).Build(),
		NetworkDetector: detector,
	}

	ctx := ctrllog.IntoContext(context.Background(), logr.Discard())
	result, err := r.reconcileCreate(ctx, logr.Discard(), svc)
	if err != nil {
		t.Fatalf("reconcileCreate returned error: %v", err)
	}
	if result.RequeueAfter != serviceRetryRequeue {
		t.Errorf("expected retry requeue after %v for detector error, got %v", serviceRetryRequeue, result.RequeueAfter)
	}
	if svc.Annotations[huaweicloud.CCEAutocreateAnnotation] != "" {
		t.Error("expected no autocreate annotation on detector error")
	}
}
