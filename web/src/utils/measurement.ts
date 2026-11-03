export type MeasurementKind = 'count' | 'cpu' | 'memory' | 'storage' | 'bytes' | 'duration';

export type Measurement = {
  amount: number | null;
  unit: string;
  valid: boolean;
};

type UnitDefinition = {value: string; factor: number};

const units: Record<MeasurementKind, UnitDefinition[]> = {
  count: [{value: 'count', factor: 1}],
  cpu: [
    {value: 'cpu', factor: 1000},
    {value: 'm', factor: 1},
  ],
  memory: [
    {value: 'Mi', factor: 1},
    {value: 'Gi', factor: 1024},
  ],
  storage: [
    {value: 'Gi', factor: 1},
    {value: 'Ti', factor: 1024},
  ],
  bytes: [
    {value: 'MiB', factor: 1},
    {value: 'GiB', factor: 1024},
    {value: 'TiB', factor: 1024 * 1024},
  ],
  duration: [
    {value: 's', factor: 1},
    {value: 'm', factor: 60},
    {value: 'h', factor: 3600},
    {value: 'd', factor: 86400},
  ],
};

export function measurementUnits(kind: MeasurementKind): string[] {
  return units[kind].map(({value}) => value);
}

export function parseMeasurement(value: string | undefined, kind: MeasurementKind): Measurement {
  const fallback = {amount: null, unit: units[kind][0].value, valid: !value};
  if (!value) return fallback;

  let match: RegExpExecArray | null;
  switch (kind) {
    case 'count':
      match = /^(\d+)$/.exec(value);
      break;
    case 'cpu':
      match = /^(\d+(?:\.\d+)?)(m)?$/.exec(value);
      break;
    case 'memory':
      match = /^(\d+(?:\.\d+)?)(Mi|Gi)$/.exec(value);
      break;
    case 'storage':
      match = /^(\d+(?:\.\d+)?)(Gi|Ti)$/.exec(value);
      break;
    case 'bytes':
      match = /^(\d+(?:\.\d+)?)(MiB|GiB|TiB)$/.exec(value);
      break;
    case 'duration':
      match = /^(\d+(?:\.\d+)?)(s|m|h)$/.exec(value);
      break;
  }
  if (!match) return {...fallback, valid: false};

  const amount = Number(match[1]);
  if (!Number.isFinite(amount)) return {...fallback, valid: false};
  let unit = match[2] || (kind === 'cpu' ? 'cpu' : 'count');
  if (kind === 'duration' && unit === 'h' && amount > 0 && amount % 24 === 0) {
    unit = 'd';
    return {amount: amount / 24, unit, valid: true};
  }
  return {amount, unit, valid: true};
}

export function formatMeasurement(amount: number | null, unit: string, kind: MeasurementKind): string {
  if (amount === null || !Number.isFinite(amount)) return '';
  const normalized = Number(amount.toFixed(6));
  if (kind === 'count') return String(Math.trunc(normalized));
  if (kind === 'cpu') return unit === 'm' ? `${normalized}m` : String(normalized);
  if (kind === 'duration') return unit === 'd' ? `${normalized * 24}h` : `${normalized}${unit}`;
  return `${normalized}${unit}`;
}

export function convertMeasurement(amount: number, from: string, to: string, kind: MeasurementKind): number {
  const fromUnit = units[kind].find(({value}) => value === from);
  const toUnit = units[kind].find(({value}) => value === to);
  if (!fromUnit || !toUnit) return amount;
  return Number(((amount * fromUnit.factor) / toUnit.factor).toFixed(6));
}

export function measurementBaseValue(value: string | undefined, kind: MeasurementKind): number | null {
  const parsed = parseMeasurement(value, kind);
  if (!parsed.valid || parsed.amount === null) return null;
  const unit = units[kind].find(({value}) => value === parsed.unit);
  return unit ? parsed.amount * unit.factor : null;
}
