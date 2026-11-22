package evaluation

import "testing"

func TestCalibrationPassesRequiredCorpus(t *testing.T) {
	t.Parallel()

	samples := make([]CalibrationSample, 50)
	for index := range samples {
		samples[index] = CalibrationSample{ExpectedPass: index >= 20, PredictedPass: index >= 20, HardFailure: index < 20}
	}
	for index := 20; index < 25; index++ {
		samples[index].PredictedPass = false
	}
	report, err := EvaluateCalibration(samples)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.AgreementRate != 0.9 || report.HardFalsePasses != 0 {
		t.Fatalf("report = %#v", report)
	}
}

func TestCalibrationRejectsHardFalsePassAndSmallCorpus(t *testing.T) {
	t.Parallel()

	samples := make([]CalibrationSample, 50)
	for index := range samples {
		samples[index] = CalibrationSample{ExpectedPass: index >= 20, PredictedPass: index >= 20, HardFailure: index < 20}
	}
	samples[0].PredictedPass = true
	report, err := EvaluateCalibration(samples)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed || report.HardFalsePasses != 1 {
		t.Fatalf("hard false pass report = %#v", report)
	}

	report, err = EvaluateCalibration(samples[:49])
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatalf("small corpus passed: %#v", report)
	}
}
