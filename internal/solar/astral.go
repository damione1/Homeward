// Adapted to Go from Astral 2.2, src/astral/sun.py, commit
// 3282d7b437349bc035c8a7dff5c27ec9997a2cdf.
// Copyright 2009-2019 Simon Kennedy. Licensed under Apache-2.0; see LICENSE.astral.
// Homeward changes: admitted private inputs, typed absence/errors, bounded work,
// and UTC value returns. Operation order and Astral 2.2 refraction are preserved.
package solar

import (
	"math"
	"strconv"
	"time"
)

func radians(v float64) float64 { return v * (math.Pi / 180) }
func degrees(v float64) float64 { return v * (180 / math.Pi) }

func julianDay(date time.Time) float64 {
	y, m, d := date.Date()
	if m <= 2 {
		y--
		m += 12
	}
	a := math.Floor(float64(y) / 100)
	b := 2 - a + math.Floor(a/4)
	return math.Floor(365.25*float64(y+4716)) + math.Floor(30.6001*float64(m+1)) + float64(d) + b - 1524.5
}
func julianCentury(day float64) float64 { return (day - 2451545) / 36525 }
func centuryDay(t float64) float64      { return t*36525 + 2451545 }
func meanLongitude(t float64) float64 {
	v := math.Mod(280.46646+t*(36000.76983+0.0003032*t), 360)
	if v < 0 {
		v += 360
	}
	return v
}
func meanAnomaly(t float64) float64 { return 357.52911 + t*(35999.05029-0.0001537*t) }
func eccentricity(t float64) float64 {
	return 0.016708634 - t*(0.000042037+0.0000001267*t)
}
func center(t float64) float64 {
	m := radians(meanAnomaly(t))
	return math.Sin(m)*(1.914602-t*(0.004817+0.000014*t)) + math.Sin(m+m)*(0.019993-0.000101*t) + math.Sin(m+m+m)*0.000289
}
func apparentLongitude(t float64) float64 {
	return meanLongitude(t) + center(t) - 0.00569 - 0.00478*math.Sin(radians(125.04-1934.136*t))
}
func obliquity(t float64) float64 {
	seconds := 21.448 - t*(46.815+t*(0.00059-t*0.001813))
	e0 := 23 + (26+seconds/60)/60
	return e0 + 0.00256*math.Cos(radians(125.04-1934.136*t))
}
func declination(t float64) float64 {
	return degrees(math.Asin(math.Sin(radians(obliquity(t))) * math.Sin(radians(apparentLongitude(t)))))
}
func equationOfTime(t float64) float64 {
	l0, e, m := meanLongitude(t), eccentricity(t), meanAnomaly(t)
	y := math.Tan(radians(obliquity(t)) / 2)
	y *= y
	sin2l0, sinm := math.Sin(2*radians(l0)), math.Sin(radians(m))
	cos2l0, sin4l0, sin2m := math.Cos(2*radians(l0)), math.Sin(4*radians(l0)), math.Sin(2*radians(m))
	value := y*sin2l0 - 2*e*sinm + 4*e*y*sinm*cos2l0 - 0.5*y*y*sin4l0 - 1.25*e*e*sin2m
	return degrees(value) * 4
}

type traversal uint8

const (
	traversalUnspecified traversal = iota
	traversalRising
	traversalSetting
)

func hourAngle(latitude, decl, zenith float64, direction traversal) (float64, bool, error) {
	lat, dec, zen := radians(latitude), radians(decl), radians(zenith)
	ratio := (math.Cos(zen) - math.Sin(lat)*math.Sin(dec)) / (math.Cos(lat) * math.Cos(dec))
	if !finite(ratio) {
		return 0, false, failure(CalculationFailed)
	}
	if ratio < -1 || ratio > 1 {
		return 0, false, nil // Astral's acos ValueError: no transit on this date.
	}
	angle := math.Acos(ratio)
	switch direction {
	case traversalRising:
		return angle, true, nil
	case traversalSetting:
		return -angle, true, nil
	case traversalUnspecified:
		return 0, false, failure(CalculationFailed)
	default:
		return 0, false, failure(CalculationFailed)
	}
}

func calculationLatitude(latitude float64) float64 {
	if latitude > 89.8 {
		return 89.8
	}
	if latitude < -89.8 {
		return -89.8
	}
	return latitude
}
func horizonAdjustment(elevation float64) float64 {
	if elevation <= 0 {
		return 0
	}
	return degrees(math.Acos(6356900 / (6356900 + elevation)))
}
func refraction(zenith float64) float64 {
	elevation := 90 - zenith
	if elevation >= 85 {
		return 0
	}
	te := math.Tan(radians(elevation))
	var correction float64
	if elevation > 5 {
		correction = 58.1/te - 0.07/(te*te*te) + 0.000086/(te*te*te*te*te)
	} else if elevation > -0.575 {
		step1 := -12.79 + elevation*0.711
		step2 := 103.4 + elevation*step1
		step3 := -518.2 + elevation*step2
		correction = 1735 + elevation*step3
	} else {
		correction = -20.774 / te
	}
	return correction / 3600
}

func minutesToTime(date time.Time, minutes float64) (time.Time, error) {
	// The admitted observer and acos range bound transit minutes to a few days.
	// Reject a numerical failure before any float-to-duration conversion.
	if !finite(minutes) || math.Abs(minutes) > 1000000 {
		return time.Time{}, failure(CalculationFailed)
	}
	days := int(minutes / 1440)
	seconds := (minutes - float64(days*1440)) * 60
	whole := int(seconds)
	micros := int((seconds - float64(whole)) * 1000000)
	result := date.AddDate(0, 0, days).Add(time.Duration(whole)*time.Second + time.Duration(micros)*time.Microsecond)
	if result.Year() < 1 || result.Year() > 9999 || result.IsZero() {
		return time.Time{}, failure(InvalidDate)
	}
	return result.UTC(), nil
}

func transit(o Observer, date time.Time, zenith float64, direction traversal) (time.Time, bool, error) {
	latitude := calculationLatitude(o.latitude)
	adjustElevation := horizonAdjustment(o.elevation)
	adjustRefraction := refraction(zenith + adjustElevation)
	t := julianCentury(julianDay(date))
	angle, present, err := hourAngle(latitude, declination(t), zenith+adjustElevation-adjustRefraction, direction)
	if err != nil || !present {
		return time.Time{}, false, err
	}
	delta := -o.longitude - degrees(angle)
	minutes := 720 + 4*delta - equationOfTime(t)
	t = julianCentury(centuryDay(t) + minutes/1440)
	// The plus sign in the second pass is intentionally Astral 2.2's rule.
	angle, present, err = hourAngle(latitude, declination(t), zenith+adjustElevation+adjustRefraction, direction)
	if err != nil || !present {
		return time.Time{}, false, err
	}
	delta = -o.longitude - degrees(angle)
	result, err := minutesToTime(date, 720+4*delta-equationOfTime(t))
	return result, err == nil, err
}

func clockParts(hours float64) (hour, minute, second int, err error) {
	if !finite(hours) || math.Abs(hours) > 1000000 {
		return 0, 0, 0, failure(CalculationFailed)
	}
	hour = int(hours)
	minute = int((hours - float64(hour)) * 60)
	second = int((((hours - float64(hour)) * 60) - float64(minute)) * 60)
	if second > 59 {
		second -= 60
		minute++
	} else if second < 0 {
		second += 60
		minute--
	}
	if minute > 59 {
		minute -= 60
		hour++
	} else if minute < 0 {
		minute += 60
		hour--
	}
	return hour, minute, second, nil
}

func meridianTime(o Observer, date time.Time, midnight bool) (time.Time, error) {
	var hours float64
	if midnight {
		t := julianCentury(julianDay(date) + 0.5 + -o.longitude/360)
		hours = (-o.longitude*4 - equationOfTime(t)) / 60
	} else {
		t := julianCentury(julianDay(date))
		hours = (720 - 4*o.longitude - equationOfTime(t)) / 60
	}
	hour, minute, second, err := clockParts(hours)
	if err != nil {
		return time.Time{}, err
	}
	if !midnight && hour > 23 {
		hour -= 24
		date = date.AddDate(0, 0, 1)
	} else if hour < 0 {
		hour += 24
		date = date.AddDate(0, 0, -1)
	}
	if date.Year() < 1 || date.Year() > 9999 || hour < 0 || hour > 23 || minute < 0 || minute > 59 || second < 0 || second > 59 {
		return time.Time{}, failure(InvalidDate)
	}
	result := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, second, 0, time.UTC)
	if result.IsZero() {
		return time.Time{}, failure(InvalidDate)
	}
	return result, nil
}

func position(o Observer, instant time.Time) (elevation, azimuth float64, err error) {
	instant = instant.UTC() // HA supplies UTC, including at local date boundaries.
	latitude := calculationLatitude(o.latitude)
	hours := float64(instant.Hour()) + float64(instant.Minute())/60 + float64(instant.Second())/3600
	t := julianCentury(julianDay(instant) + hours/24)
	dec, eqtime := declination(t), equationOfTime(t)
	trueSolarTime := float64(instant.Hour())*60 + float64(instant.Minute()) + float64(instant.Second())/60 + eqtime + 4*o.longitude
	for trueSolarTime > 1440 {
		trueSolarTime -= 1440
	}
	angle := trueSolarTime/4 - 180
	if angle < -180 {
		angle += 360
	}
	lat, decl := radians(latitude), radians(dec)
	csz := math.Sin(lat)*math.Sin(decl) + math.Cos(lat)*math.Cos(decl)*math.Cos(radians(angle))
	csz = math.Max(-1, math.Min(1, csz))
	zenith := degrees(math.Acos(csz))
	denominator := math.Cos(lat) * math.Sin(radians(zenith))
	if math.Abs(denominator) > 0.001 {
		ratio := (math.Sin(lat)*math.Cos(radians(zenith)) - math.Sin(decl)) / denominator
		ratio = math.Max(-1, math.Min(1, ratio))
		azimuth = 180 - degrees(math.Acos(ratio))
		if angle > 0 {
			azimuth = -azimuth
		}
	} else if latitude > 0 {
		azimuth = 180
	}
	if azimuth < 0 {
		azimuth += 360
	}
	zenith -= refraction(zenith)
	elevation = 90 - zenith
	if !finite(elevation) || !finite(azimuth) {
		return 0, 0, failure(CalculationFailed)
	}
	return elevation, azimuth, nil
}

// Python round(x, 2) rounds the actual binary float to decimal, ties to even.
// Formatting the binary value avoids an extra, lossy multiply-by-100 step.
func roundHA2(value float64) (float64, error) {
	if !finite(value) {
		return 0, failure(CalculationFailed)
	}
	result, err := strconv.ParseFloat(strconv.FormatFloat(value, 'f', 2, 64), 64)
	if err != nil {
		return 0, failure(CalculationFailed)
	}
	return result, nil
}
