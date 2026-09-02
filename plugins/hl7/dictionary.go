package hl7

import "strings"

// This file is a deliberately small HL7 dictionary, not an attempt at a
// complete one.
//
// Field *positions* are what make a captured message readable - PID-5 is
// unambiguous whether or not anything here knows it is the patient name - so
// the names are a convenience layered on top and never a prerequisite. A
// segment or a field with no entry renders exactly as well, minus a label. That
// is why this is a static map of the segments people actually stare at rather
// than a generated table of all of HL7 v2: a full dictionary is a data-curation
// project, and this plugin's value does not depend on it.
//
// Z-segments (locally defined, starting with Z) are unknowable by definition
// and are the reason no view may ever require a name to exist.

// segmentNames covers the segments a person is likely to meet in captured
// traffic: the ADT, ORU and ORM backbone, plus the ones an acknowledgement is
// made of.
var segmentNames = map[string]string{
	"MSH": "Message Header",
	"MSA": "Message Acknowledgment",
	"ERR": "Error",
	"EVN": "Event Type",
	"PID": "Patient Identification",
	"PD1": "Patient Additional Demographic",
	"NK1": "Next of Kin / Associated Parties",
	"PV1": "Patient Visit",
	"PV2": "Patient Visit - Additional Information",
	"AL1": "Patient Allergy Information",
	"DG1": "Diagnosis",
	"DRG": "Diagnosis Related Group",
	"PR1": "Procedures",
	"GT1": "Guarantor",
	"IN1": "Insurance",
	"IN2": "Insurance Additional Information",
	"IN3": "Insurance Additional Information, Certification",
	"ACC": "Accident",
	"UB1": "UB82 Data",
	"UB2": "UB92 Data",
	"ORC": "Common Order",
	"OBR": "Observation Request",
	"OBX": "Observation / Result",
	"NTE": "Notes and Comments",
	"SPM": "Specimen",
	"TQ1": "Timing / Quantity",
	"RXA": "Pharmacy / Treatment Administration",
	"RXE": "Pharmacy / Treatment Encoded Order",
	"RXR": "Pharmacy / Treatment Route",
	"SCH": "Scheduling Activity Information",
	"AIS": "Appointment Information - Service",
	"AIL": "Appointment Information - Location",
	"AIP": "Appointment Information - Personnel",
	"FHS": "File Header",
	"FTS": "File Trailer",
	"BHS": "Batch Header",
	"BTS": "Batch Trailer",
	"QRD": "Query Definition",
	"QRF": "Query Filter",
	"QAK": "Query Acknowledgment",
	"QPD": "Query Parameter Definition",
	"RCP": "Response Control Parameter",
	"DSC": "Continuation Pointer",
}

// fieldNames names the fields of the segments worth reading closely. The list
// stops where usefulness does: the tail of a segment is mostly rarely-populated
// administrative fields, and a wrong label is worse than none at all.
var fieldNames = map[string][]string{
	"MSH": {
		"Field Separator", "Encoding Characters", "Sending Application", "Sending Facility",
		"Receiving Application", "Receiving Facility", "Date/Time of Message", "Security",
		"Message Type", "Message Control ID", "Processing ID", "Version ID",
		"Sequence Number", "Continuation Pointer", "Accept Acknowledgment Type",
		"Application Acknowledgment Type", "Country Code", "Character Set",
		"Principal Language of Message", "Alternate Character Set Handling Scheme",
		"Message Profile Identifier",
	},
	"MSA": {
		"Acknowledgment Code", "Message Control ID", "Text Message",
		"Expected Sequence Number", "Delayed Acknowledgment Type", "Error Condition",
	},
	"EVN": {
		"Event Type Code", "Recorded Date/Time", "Date/Time Planned Event",
		"Event Reason Code", "Operator ID", "Event Occurred", "Event Facility",
	},
	"PID": {
		"Set ID", "Patient ID", "Patient Identifier List", "Alternate Patient ID",
		"Patient Name", "Mother's Maiden Name", "Date/Time of Birth", "Administrative Sex",
		"Patient Alias", "Race", "Patient Address", "County Code", "Phone Number - Home",
		"Phone Number - Business", "Primary Language", "Marital Status", "Religion",
		"Patient Account Number", "SSN Number", "Driver's License Number",
		"Mother's Identifier", "Ethnic Group", "Birth Place", "Multiple Birth Indicator",
		"Birth Order", "Citizenship", "Veterans Military Status", "Nationality",
		"Patient Death Date and Time", "Patient Death Indicator",
	},
	"PV1": {
		"Set ID", "Patient Class", "Assigned Patient Location", "Admission Type",
		"Preadmit Number", "Prior Patient Location", "Attending Doctor", "Referring Doctor",
		"Consulting Doctor", "Hospital Service", "Temporary Location",
		"Preadmit Test Indicator", "Re-admission Indicator", "Admit Source",
		"Ambulatory Status", "VIP Indicator", "Admitting Doctor", "Patient Type",
		"Visit Number", "Financial Class",
	},
	"OBR": {
		"Set ID", "Placer Order Number", "Filler Order Number", "Universal Service Identifier",
		"Priority", "Requested Date/Time", "Observation Date/Time", "Observation End Date/Time",
		"Collection Volume", "Collector Identifier", "Specimen Action Code", "Danger Code",
		"Relevant Clinical Information", "Specimen Received Date/Time", "Specimen Source",
		"Ordering Provider", "Order Callback Phone Number", "Placer Field 1", "Placer Field 2",
		"Filler Field 1", "Filler Field 2", "Results Report/Status Change Date/Time",
		"Charge to Practice", "Diagnostic Service Section ID", "Result Status",
		"Parent Result", "Quantity/Timing", "Result Copies To",
	},
	"OBX": {
		"Set ID", "Value Type", "Observation Identifier", "Observation Sub-ID",
		"Observation Value", "Units", "References Range", "Abnormal Flags", "Probability",
		"Nature of Abnormal Test", "Observation Result Status",
		"Effective Date of Reference Range", "User Defined Access Checks",
		"Date/Time of the Observation", "Producer's ID", "Responsible Observer",
		"Observation Method", "Equipment Instance Identifier", "Date/Time of the Analysis",
	},
	"NTE": {"Set ID", "Source of Comment", "Comment", "Comment Type"},
	"ORC": {
		"Order Control", "Placer Order Number", "Filler Order Number", "Placer Group Number",
		"Order Status", "Response Flag", "Quantity/Timing", "Parent Order",
		"Date/Time of Transaction", "Entered By", "Verified By", "Ordering Provider",
	},
	"ERR": {
		"Error Code and Location", "Error Location", "HL7 Error Code", "Severity",
		"Application Error Code", "Application Error Parameter", "Diagnostic Information",
		"User Message",
	},
}

// SegmentName is the human name of a segment id, or "" when the dictionary does
// not know it - which a view must treat as normal, not as an error.
func SegmentName(id string) string { return segmentNames[strings.ToUpper(id)] }

// FieldName is the human name of a 1-based field position within a segment, or
// "" when it is not in the dictionary.
func FieldName(segment string, position int) string {
	names := fieldNames[strings.ToUpper(segment)]
	if position < 1 || position > len(names) {
		return ""
	}
	return names[position-1]
}
