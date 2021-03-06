// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package order provides functionality to generate order and set results.
package order

import (
	"fmt"
	"time"

	"github.com/pkg/errors"
	"github.com/google/simhospital/pkg/config"
	"github.com/google/simhospital/pkg/constants"
	"github.com/google/simhospital/pkg/doctor"
	"github.com/google/simhospital/pkg/generator/id"
	"github.com/google/simhospital/pkg/logging"
	"github.com/google/simhospital/pkg/message"
	"github.com/google/simhospital/pkg/orderprofile"
	"github.com/google/simhospital/pkg/pathway"
)

var log = logging.ForCallerPackage()

// NotesGenerator is an interface to generate notes for results and clinical notes.
type NotesGenerator interface {
	// RandomNotesForResult generates textual notes for a Result, to be set in NTE segments related to the result.
	RandomNotesForResult() []string
	// RandomDocumentForClinicalNote generates a document that contains a clinical note.
	RandomDocumentForClinicalNote(*pathway.ClinicalNote, *message.ClinicalNote, time.Time) (*message.ClinicalNote, error)
}

// Generator is a generator of orders and results.
type Generator struct {
	MessageConfig         *config.HL7Config
	OrderProfiles         *orderprofile.OrderProfiles
	NoteGenerator         NotesGenerator
	PlacerGenerator       id.Generator
	FillerGenerator       id.Generator
	AbnormalFlagConvertor AbnormalFlagConvertor
	Doctors               *doctor.Doctors
}

// NewOrder returns a new order based on order information from the pathway and eventTime.
func (g Generator) NewOrder(o *pathway.Order, eventTime time.Time) *message.Order {
	orderStatus := o.OrderStatus
	if orderStatus == "" {
		orderStatus = g.MessageConfig.OrderStatus.InProcess
	}
	return &message.Order{
		OrderProfile:  g.OrderProfiles.Generate(o.OrderProfile),
		Placer:        g.PlacerGenerator.NewID(),
		OrderDateTime: message.NewValidTime(eventTime),
		OrderControl:  g.MessageConfig.OrderControl.New,
		OrderStatus:   orderStatus,
	}
}

// OrderWithClinicalNote updates an order with a Clinical Note. If the supplied order is nil, a new order is created.
// This order will contain a single result with the Clinical Note generated/updated based on the pathway.
// The DiagnosticServID section is set to DiagnosticServIDMDOC, which indicates that the corresponding HL7 is a Clinical Note.
func (g Generator) OrderWithClinicalNote(order *message.Order, n *pathway.ClinicalNote, eventTime time.Time) (*message.Order, error) {
	var existingNote *message.ClinicalNote
	if order != nil {
		if len(order.Results) != 1 {
			return nil, errors.New("No results found in the provided order; expected 1")
		}
		if order.Results[0].ClinicalNote == nil {
			return nil, errors.New("Order is not a Clinical Note order")
		}
		existingNote = order.Results[0].ClinicalNote
	}

	note, err := g.NoteGenerator.RandomDocumentForClinicalNote(n, existingNote, eventTime)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate a random note")
	}

	if order == nil {
		order = &message.Order{
			ResultsStatus:    g.MessageConfig.DocumentStatus.Authenticated,
			OrderingProvider: g.Doctors.GetRandomDoctor(),
			DiagnosticServID: message.DiagnosticServIDMDOC,
		}
	}

	// Order.OrderProfile gets rendered to the Universal Service Identifier field of an OBR segment (OBR.4) as
	// OrderProfile.ID^OrderProfile.Text^OrderProfile.CodingSystem. In clinical notes this field is used to send the
	// DocumentType, which is rendered as  DocumentType^DocumentType^^^DocumentTitle.
	order.OrderProfile = &message.CodedElement{ID: note.DocumentType, Text: note.DocumentType, AlternateText: note.DocumentTitle}
	order.Results = []*message.Result{{ClinicalNote: note}}
	return order, nil
}

// SetResults sets results on an existing Order.
// If order is nil, this first creates an Order using details in pathway.Result.
//
// If the results are explicitly specified in the pathway, only those results are included.
// If the results are not set explicitly, the results for each TestType for the given OrderProfile are included,
//
// If the Order already has the Results, they are replaced with Results from the pathway as the corrected results,
// unless another status is explicitly specified in the pathway.
// In the case of correction, only results specified in the pathway are included.
func (g Generator) SetResults(o *message.Order, r *pathway.Results, eventTime time.Time) (*message.Order, error) {
	if o == nil {
		o = g.NewOrder(&pathway.Order{OrderProfile: r.OrderProfile}, eventTime)
	}
	if o.Filler == "" {
		o.Filler = g.FillerGenerator.NewID()
	}

	g.setOrderStatuses(o, r)
	if err := g.setOrderDates(o, r, eventTime); err != nil {
		return nil, errors.Wrap(err, "cannot set dates on the order")
	}
	if err := g.setOrderResults(o, r); err != nil {
		return nil, errors.Wrap(err, "cannot set results on the order")
	}

	return o, nil
}

// setOrderStatuses sets OrderStatus and ResultsStatus of the given order based on the pathway.Result.
// If OrderStatus and ResultsStatus are explicitly specified in the pathway, they are being used.
// If the Order has some previous results with the status Final or Corrected,
// the new results are treated as a correction, so OrderStatus is set to Completed,
// and ResultsStatus is set to Corrected.
// Otherwise, the new results are treated as a final result, so OrderStatus is set to Completed,
// and ResultsStatus is set to Final.
func (g Generator) setOrderStatuses(o *message.Order, r *pathway.Results) {
	switch {
	case r.OrderStatus != "":
		// Use status value overridden in the pathway.
		// NOTE: if OrderStatus is set, ResultStatus is set too, or else the pathway would fail the validation.
		o.OrderStatus = r.OrderStatus
		o.ResultsStatus = r.ResultStatus
	case len(o.Results) > 0 && (o.ResultsStatus == g.MessageConfig.ResultStatus.Final || o.ResultsStatus == g.MessageConfig.ResultStatus.Corrected):
		// If the results were previously set to "F" (Final) or "C" (Corrected), this is a correction.
		o.OrderStatus = g.MessageConfig.OrderStatus.Completed
		o.ResultsStatus = g.MessageConfig.ResultStatus.Corrected
	default:
		// Otherwise this is a final result.
		// NOTE: there might be some results for this order already, with a different status, eg.: Preliminary.
		o.OrderStatus = g.MessageConfig.OrderStatus.Completed
		o.ResultsStatus = g.MessageConfig.ResultStatus.Final
	}
}

// setOrderDates sets CollectedDateTime, ReceivedInLabDateTime and ReportedDateTime dates
// of the given order based on the pathway.Result.
// ReportedDateTime is always set to the event time.
// CollectedDateTime and ReceivedInLabDateTime are only initialized, if there are no previous results
// for this order. The dates are set so that:
// order time <= collected time <= received in lab time <= reported time
// If CollectedDateTime or ReceivedInLabDateTime are specified explicitly in the pathway,
// then they override the order dates.
func (g Generator) setOrderDates(o *message.Order, r *pathway.Results, eventTime time.Time) error {
	o.ReportedDateTime = message.NewValidTime(eventTime)
	// If this is the first Result for this order, also set CollectedDateTime and ReceivedInLabDateTime.
	if len(o.Results) == 0 {
		// order time <= collected time <= received in lab time <= reported time
		// 1) To calculate collected time time:
		//    - get the difference between order and report time
		//    - select random delay from it.
		orderToCollectedDelay := pathway.Delay{From: 0, To: eventTime.Sub(o.OrderDateTime.Time)}
		o.CollectedDateTime = message.NewValidTime(o.OrderDateTime.Add(orderToCollectedDelay.Random()))

		// 2) To calculate received in lab time:
		//    - get the difference between collected and reported time
		//    - select random delay from it.
		collectedToReceivedInLabDelay := pathway.Delay{From: 0, To: eventTime.Sub(o.CollectedDateTime.Time)}
		o.ReceivedInLabDateTime = message.NewValidTime(o.CollectedDateTime.Time.Add(collectedToReceivedInLabDelay.Random()))
	}

	// Override dates if specified in the pathway.
	if r.CollectedDateTime != "" {
		collected, err := overriddenDate(r.CollectedDateTime, o.CollectedDateTime)
		if err != nil {
			return errors.Wrapf(err, "cannot override CollectedDateTime with %v", r.CollectedDateTime)
		}
		o.CollectedDateTime = collected
	}
	if r.ReceivedInLabDateTime != "" {
		received, err := overriddenDate(r.ReceivedInLabDateTime, o.ReceivedInLabDateTime)
		if err != nil {
			return errors.Wrapf(err, "cannot override ReceivedInLabDateTime with %v", r.ReceivedInLabDateTime)
		}
		o.ReceivedInLabDateTime = received
	}

	return nil
}

// setOrderResults sets results of the given order based on the pathway.Results.
// If the results are defined for an existing Order Profile, then:
// - if the results are explicitly specified in the pathway, only those are included,
// - if the results are not specified explicitly, then random result from the normal range
//   is included for each test type specified in the Order Profile.
// Otherwise, if the results are defined for non-existing order profile, then
// only results specified explicitly are included.
func (g Generator) setOrderResults(o *message.Order, r *pathway.Results) error {
	o.Results = make([]*message.Result, 0)
	opName := o.OrderProfile.Text
	op, ok := g.OrderProfiles.Get(opName)

	switch {
	case ok && len(r.Results) == 0:
		// Include a result for each test type specified in the order profile.
		for _, tt := range op.TestTypes {
			placeholder := &pathway.Result{
				TestName: tt.Name.Text,
				Value:    constants.NormalValue,
			}
			tr, err := g.testResult(op, placeholder, o.ResultsStatus, o.CollectedDateTime)
			if err != nil {
				return errors.Wrap(err, "cannot generate test result")
			}
			o.Results = append(o.Results, tr)
		}

	case !ok:
		log.WithField("order_profile", r.OrderProfile).
			Warningf("Order profile %q not defined; adding %d test results from the pathway", opName, len(r.Results))
		fallthrough
	default:
		// If Results are explicitly specified in the pathway, only include those.
		// Note, that for corrections we currently only include corrected values.
		for _, result := range r.Results {
			tr, err := g.testResult(op, result, o.ResultsStatus, o.CollectedDateTime)
			if err != nil {
				return errors.Wrap(err, "cannot generate test result")
			}
			o.Results = append(o.Results, tr)
		}
	}
	return nil
}

func overriddenDate(fromPathway string, t message.NullTime) (message.NullTime, error) {
	switch fromPathway {
	case constants.EmptyString:
		return message.NewInvalidTime(), nil
	case constants.MidnightDate:
		return message.NewMidnightTime(t.Time), nil
	default:
		// This can never happen if the pathway is valid.
		return t, fmt.Errorf("unknown date: %v", fromPathway)
	}
}

// testResult generates the Result from the default values in the Test Type,
// overridden by values specified in the pathway, if provided.
// If the Test Type is not provided, creates the Result from values specified in the pathway.
func (g Generator) testResult(op *orderprofile.OrderProfile, pathwayResult *pathway.Result, orderResultsStatus string, orderCollectedDateTime message.NullTime) (*message.Result, error) {
	obsDateTime := message.NewInvalidTime()
	if orderCollectedDateTime.Valid {
		obsDateTime = message.NewValidTime(orderCollectedDateTime.Add(pathwayResult.ObservationDateTimeOffset))
	}

	// Init default Result values.
	result := &message.Result{
		Status:              orderResultsStatus,
		ObservationDateTime: obsDateTime,
	}

	if len(pathwayResult.Notes) > 0 {
		result.Notes = pathwayResult.Notes
	} else {
		result.Notes = g.NoteGenerator.RandomNotesForResult()
	}

	if pathwayResult != nil && pathwayResult.ResultStatus != "" {
		result.Status = pathwayResult.ResultStatus
	}

	if op == nil {
		if err := g.updateResultForCustomOrderProfile(result, pathwayResult); err != nil {
			return nil, errors.Wrap(err, "cannot set the value on the result with custom order profile")
		}
		return result, nil
	}

	tt := op.TestTypes[pathwayResult.TestName]
	if tt == nil {
		// This shouldn't happen if the pathway has been validated.
		return nil, fmt.Errorf("Test name %q not found in order profile", pathwayResult.TestName)
	}
	// Set defaults for Test Type.
	result.TestName = &tt.Name
	result.ValueType = tt.ValueType
	result.Range = tt.RefRange
	if pathwayResult.ID != "" {
		result.TestName.ID = pathwayResult.ID
	}

	if err := g.setTestResultValue(result, pathwayResult, tt); err != nil {
		return nil, errors.Wrap(err, "cannot set the value on the result")
	}
	return result, nil
}

func (g Generator) updateResultForCustomOrderProfile(result *message.Result, pathwayResult *pathway.Result) error {
	// Set values based on the pathway.
	id := pathwayResult.ID
	if id == "" {
		id = pathwayResult.TestName
	}
	result.TestName = &message.CodedElement{ID: id, Text: pathwayResult.TestName}
	result.ValueType = pathwayResult.GetValueType()

	switch {
	case pathwayResult.IsValueRandom() && pathwayResult.ReferenceRange != "":
		if err := g.setRandomValueBasedOnCustomReferenceRange(result, pathwayResult); err != nil {
			return errors.Wrap(err, "cannot generate random value from custom reference range")
		}
		return nil

	default:
		af, err := pathwayResult.GetAbnormalFlag(nil)
		if err != nil {
			return errors.Wrap(err, "cannot get abnormal flag from nil generator")
		}
		result.Value = pathwayResult.GetValue()
		result.Unit = pathwayResult.GetUnit()
		result.Range = pathwayResult.ReferenceRange
		result.AbnormalFlag = g.AbnormalFlagConvertor.ToHL7(af)
		return nil
	}
}

// setTestResultValue sets Value, Unit, Range and AbnormalFlag values on the given result
// based on the pathway.Result and TestType provided.
// If the value of the results is set to random and the reference range is explicitly
// specified in the pathway, then the value is generated based on that reference range.
// If the value of the results is set to random but the reference range is not
// specified in the pathway, then the value is generated based on that reference range
// specified in the TestType.
// Otherwise, if the value is explicitly set in the pathway, it is being used.
func (g Generator) setTestResultValue(result *message.Result, pathwayResult *pathway.Result, tt *orderprofile.TestType) error {
	switch {
	case pathwayResult.IsValueRandom() && pathwayResult.ReferenceRange != "":
		// Generate random value from the custom reference range.
		return g.setRandomValueBasedOnCustomReferenceRange(result, pathwayResult)

	case pathwayResult.IsValueRandom() && pathwayResult.ReferenceRange == "":
		// Generate random value from the order profile's reference range.
		return g.setRandomValueBasedOnOrderProfileReferenceRange(result, pathwayResult, tt)

	default:
		// Use values specified in the pathway.
		return g.setValueSpecifiedInThePathway(result, pathwayResult, tt)
	}
	return nil
}

func (g Generator) setRandomValueBasedOnCustomReferenceRange(result *message.Result, pathwayResult *pathway.Result) error {
	rt, err := pathwayResult.GetRandomType()
	if err != nil {
		return errors.Wrap(err, "cannot get random type for result")
	}
	vg, err := orderprofile.ValueGeneratorFromRange(pathwayResult.ReferenceRange)
	if err != nil {
		// This should never happen if the pathway is valid.
		return errors.Wrapf(err, "cannot create value generator for reference range %q", pathwayResult.ReferenceRange)
	}
	result.Value, _ = vg.Random(rt)
	result.Unit = pathwayResult.Unit
	result.Range = pathwayResult.ReferenceRange
	result.AbnormalFlag = g.AbnormalFlagConvertor.ToHL7(constants.FromRandomType(rt))
	return nil
}

func (g Generator) setRandomValueBasedOnOrderProfileReferenceRange(result *message.Result, pathwayResult *pathway.Result, tt *orderprofile.TestType) error {
	rt, err := pathwayResult.GetRandomType()
	if err != nil {
		return errors.Wrap(err, "cannot get random type for result")
	}
	v, af, err := tt.RandomisedValueWithFlag(rt)
	if err != nil {
		return errors.Wrap(err, "cannot generate random result with abnormal flag")
	}
	result.Value = v
	result.Unit = tt.Unit
	result.AbnormalFlag = g.AbnormalFlagConvertor.ToHL7(af)
	return nil
}

func (g Generator) setValueSpecifiedInThePathway(result *message.Result, pathwayResult *pathway.Result, tt *orderprofile.TestType) error {
	result.Value = pathwayResult.GetValue()
	result.Unit = pathwayResult.GetUnit()
	// It's not always possible to derive the type from the value, e.g., a value of an empty string doesn't necessarily mean
	// that the type is textual: value="" and valueType="NM" is a valid case. In that case, default to the type from the order profile.
	// Also default to the order profile one if both value types are textual, as we assume that the order profile is more precise.
	if vt := pathwayResult.GetValueType(); vt != "" && (vt == constants.NumericalValueType || tt.ValueType == constants.NumericalValueType) {
		result.ValueType = vt
	}
	if pathwayResult.ReferenceRange != "" {
		result.Range = pathwayResult.ReferenceRange
	}
	abnormalFlag, err := pathwayResult.GetAbnormalFlag(tt.ValueGenerator)
	if err != nil {
		return errors.Wrap(err, "cannot get abnormal flag")
	}
	result.AbnormalFlag = g.AbnormalFlagConvertor.ToHL7(abnormalFlag)
	return nil
}
