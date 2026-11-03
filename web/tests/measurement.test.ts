import {expect, test} from '@playwright/test';
import {convertMeasurement, formatMeasurement, measurementBaseValue, parseMeasurement} from '../src/utils/measurement';

test('measurement values round-trip through the supported display units', () => {
  expect(parseMeasurement('500m', 'cpu')).toEqual({amount: 500, unit: 'm', valid: true});
  expect(convertMeasurement(1, 'cpu', 'm', 'cpu')).toBe(1000);
  expect(formatMeasurement(1000, 'm', 'cpu')).toBe('1000m');
  expect(formatMeasurement(2, 'Gi', 'memory')).toBe('2Gi');
  expect(measurementBaseValue('2Ti', 'storage')).toBe(2048);
  expect(measurementBaseValue('1.5GiB', 'bytes')).toBe(1536);
  expect(parseMeasurement('720h', 'duration')).toEqual({amount: 30, unit: 'd', valid: true});
  expect(formatMeasurement(30, 'd', 'duration')).toBe('720h');
  expect(convertMeasurement(30, 'm', 'h', 'duration')).toBe(0.5);
  expect(formatMeasurement(0.5, 'h', 'duration')).toBe('0.5h');
});

test('measurement parsing rejects mixed or unsupported values', () => {
  expect(parseMeasurement('10Gi text', 'storage').valid).toBe(false);
  expect(parseMeasurement('-1', 'count').valid).toBe(false);
  expect(measurementBaseValue('1e3m', 'cpu')).toBeNull();
});
