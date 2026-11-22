package evaluation

import "fmt"

const (
	minimumCalibrationSamples   = 50
	minimumHardFailureSamples   = 20
	minimumCalibrationAgreement = 0.90
)

type CalibrationSample struct {
	ExpectedPass  bool
	PredictedPass bool
	HardFailure   bool
}

type CalibrationReport struct {
	Passed           bool
	SampleCount      int
	HardFailureCount int
	AgreementRate    float64
	HardFalsePasses  int
}

// EvaluateCalibration applies the mandatory Judge activation gate.
func EvaluateCalibration(samples []CalibrationSample) (CalibrationReport, error) {
	if len(samples) == 0 {
		return CalibrationReport{}, fmt.Errorf("calibration samples are required")
	}
	report := CalibrationReport{SampleCount: len(samples)}
	agreements := 0
	for _, sample := range samples {
		if sample.ExpectedPass == sample.PredictedPass {
			agreements++
		}
		if sample.HardFailure {
			report.HardFailureCount++
			if sample.PredictedPass {
				report.HardFalsePasses++
			}
		}
	}
	report.AgreementRate = float64(agreements) / float64(len(samples))
	report.Passed = report.SampleCount >= minimumCalibrationSamples &&
		report.HardFailureCount >= minimumHardFailureSamples &&
		report.AgreementRate >= minimumCalibrationAgreement &&
		report.HardFalsePasses == 0
	return report, nil
}
