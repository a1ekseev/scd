export class AppError extends Error {
  readonly code: string;

  constructor(code: string, message: string, options?: ErrorOptions) {
    super(message, options);
    this.code = code;
    this.name = this.constructor.name;
  }
}

export class ConfigError extends AppError {
  constructor(message: string, options?: ErrorOptions) {
    super('CONFIG_ERROR', message, options);
  }
}

export class ApiRequestError extends AppError {
  constructor(message: string, options?: ErrorOptions) {
    super('API_REQUEST_ERROR', message, options);
  }
}

export class InputError extends AppError {
  constructor(code: 'EMPTY_INPUT', message: string, options?: ErrorOptions) {
    super(code, message, options);
  }
}

export class ManifestValidationError extends AppError {
  constructor(message: string, options?: ErrorOptions) {
    super('NO_VALID_ENTRIES', message, options);
  }
}
