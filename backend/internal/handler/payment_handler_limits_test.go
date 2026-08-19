package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

func TestApplyConfiguredRechargeFloor(t *testing.T) {
	tests := []struct {
		name        string
		methodFloor float64
		appFloor    float64
		want        float64
	}{
		{name: "application floor wins", methodFloor: 0, appFloor: 10, want: 10},
		{name: "method floor wins", methodFloor: 20, appFloor: 10, want: 20},
		{name: "higher configured floor is preserved", methodFloor: 10, appFloor: 50, want: 50},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			limits := &service.MethodLimitsResponse{GlobalMin: tt.methodFloor}
			applyConfiguredRechargeFloor(limits, &service.PaymentConfig{MinAmount: tt.appFloor})
			if limits.GlobalMin != tt.want {
				t.Fatalf("global minimum = %v, want %v", limits.GlobalMin, tt.want)
			}
		})
	}
}
