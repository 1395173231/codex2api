package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestMutateModelPricingSettingsSerializesReadMergeWrite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "pricing.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		_, err := db.MutateModelPricingSettings(context.Background(), nil, func(current map[string]ModelPricingOverride) error {
			close(firstEntered)
			<-releaseFirst
			current["gpt-first"] = ModelPricingOverride{Source: ModelPricingSourceCustom, Input: 1}
			return nil
		})
		firstDone <- err
	}()
	<-firstEntered

	secondDone := make(chan error, 1)
	go func() {
		_, err := db.MutateModelPricingSettings(context.Background(), nil, func(current map[string]ModelPricingOverride) error {
			current["gpt-second"] = ModelPricingOverride{Source: ModelPricingSourceSynced, Input: 2}
			return nil
		})
		secondDone <- err
	}()

	select {
	case err := <-secondDone:
		t.Fatalf("second mutation bypassed coordinator: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first mutation: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second mutation: %v", err)
	}

	settings, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	overrides, err := ParseModelPricingOverridesJSON(settings.ModelPricingOverrides)
	if err != nil {
		t.Fatalf("ParseModelPricingOverridesJSON: %v", err)
	}
	if overrides["gpt-first"].Input != 1 || overrides["gpt-second"].Input != 2 {
		t.Fatalf("serialized mutations lost data: %+v", overrides)
	}
}
