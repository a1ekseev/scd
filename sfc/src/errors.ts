export class ConfigError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ConfigError';
  }
}

export class InputError extends Error {
  readonly code: 'EMPTY_INPUT';

  constructor(
    code: 'EMPTY_INPUT',
    message: string,
  ) {
    super(message);
    this.code = code;
    this.name = 'InputError';
  }
}
