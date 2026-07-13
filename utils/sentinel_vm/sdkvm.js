'use strict';

/**
 * Readable reimplementation of the `_n` VM found in `sssdk.js`.
 *
 * Core model:
 * 1. A single `Map` stores both VM registers and opcode handlers.
 * 2. The program is a JSON array of instructions: `[opcode, ...args]`.
 * 3. The encrypted payload is `btoa(JSON.stringify(program))`, then XOR'd with a secret.
 * 4. Opcode `3` resolves, opcode `4` rejects, opcode `30` builds VM-backed JS closures.
 *
 * The original bundle keeps all state in shared globals and serializes calls through a
 * promise chain. This version keeps the machine state per interpreter instance, but keeps
 * the same wire protocol so it can be used as a drop-in decoder / executor.
 */

const carrierSecrets = new WeakMap();

const OPCODES = Object.freeze({
  RUN_ENCODED_PROGRAM: 0,
  XOR_IN_PLACE: 1,
  SET_LITERAL: 2,
  RESOLVE: 3,
  REJECT: 4,
  APPEND_OR_ADD: 5,
  READ_PROPERTY: 6,
  CALL_WITH_REGISTER_ARGS: 7,
  COPY_REGISTER: 8,
  INSTRUCTION_QUEUE: 9,
  WINDOW_OBJECT: 10,
  FIND_SCRIPT_SRC_BY_REGEX: 11,
  STORE_REGISTER_MAP: 12,
  CALL_WITH_RAW_ARGS: 13,
  JSON_PARSE: 14,
  JSON_STRINGIFY: 15,
  SECRET_KEY: 16,
  CALL_AND_STORE_RESULT: 17,
  BASE64_DECODE_IN_PLACE: 18,
  BASE64_ENCODE_IN_PLACE: 19,
  CALL_IF_EQUAL: 20,
  CALL_IF_DELTA_EXCEEDS: 21,
  RUN_BLOCK: 22,
  CALL_IF_DEFINED: 23,
  BIND_METHOD: 24,
  NOOP_25: 25,
  NOOP_26: 26,
  REMOVE_OR_SUBTRACT: 27,
  NOOP_28: 28,
  LESS_THAN: 29,
  DEFINE_VM_FUNCTION: 30,
  MULTIPLY: 33,
  AWAIT_VALUE: 34,
  DIVIDE: 35
});

function bindSecretToCarrier(carrier, secret) {
  if (carrier && (typeof carrier === 'object' || typeof carrier === 'function')) {
    carrierSecrets.set(carrier, String(secret ?? ''));
  }
  return carrier;
}

function createSecretCarrier(secret) {
  return bindSecretToCarrier({}, secret);
}

function Tn(text, secret) {
  return xorCipher(text, secret);
}

function resolveSecretInput(secretInput) {
  if (secretInput && (typeof secretInput === 'object' || typeof secretInput === 'function')) {
    return carrierSecrets.get(secretInput) ?? '';
  }
  return String(secretInput ?? '');
}

function xorCipher(text, secret) {
  const source = String(text ?? '');
  const key = String(secret ?? '');

  if (key.length === 0) {
    return source;
  }

  let output = '';
  for (let index = 0; index < source.length; index += 1) {
    output += String.fromCharCode(
      source.charCodeAt(index) ^ key.charCodeAt(index % key.length)
    );
  }
  return output;
}

function defaultAtob(value) {
  if (typeof atob === 'function') {
    return atob(value);
  }
  return Buffer.from(String(value), 'base64').toString('binary');
}

function defaultBtoa(value) {
  if (typeof btoa === 'function') {
    return btoa(value);
  }
  return Buffer.from(String(value), 'binary').toString('base64');
}

function isPromiseLike(value) {
  return Boolean(value) && typeof value.then === 'function';
}

function toErrorText(error) {
  return String(error);
}

function isBase64Like(value, atobImpl, btoaImpl) {
  if (typeof value !== 'string' || value.length === 0 || value.length % 4 !== 0) {
    return false;
  }

  if (!/^[A-Za-z0-9+/]+=*$/.test(value)) {
    return false;
  }

  try {
    return btoaImpl(atobImpl(value)) === value;
  } catch {
    return false;
  }
}

function decodeInstructionSetFromInputs(secretInput, encodedPayload, options = {}) {
  return ReadableSssdkInterpreter.decodeInstructionSetFromInputs(
    secretInput,
    encodedPayload,
    options
  );
}

async function runFromInputs(secretInput, encodedPayload, options = {}) {
  return ReadableSssdkInterpreter.runFromInputs(secretInput, encodedPayload, options);
}

class ReadableSssdkInterpreter {
  constructor(options = {}) {
    this.windowRef = options.windowRef ?? globalThis.window ?? globalThis;
    this.documentRef = options.documentRef ?? globalThis.document ?? { scripts: [] };
    this.atob = options.atobImpl ?? defaultAtob;
    this.btoa = options.btoaImpl ?? defaultBtoa;
    this.defaultTimeoutMs = options.timeoutMs ?? 500;
    this.hooks = options.hooks ?? {};

    this.registers = new Map();
    this.stepCount = 0;
    this.executionTail = Promise.resolve();
  }

  static bindSecretToCarrier(carrier, secret) {
    return bindSecretToCarrier(carrier, secret);
  }

  static createSecretCarrier(secret) {
    return createSecretCarrier(secret);
  }

  static Tn(text, secret) {
    return Tn(text, secret);
  }

  static decodeInstructionSetFromXorValues(xorSourceText, secret) {
    return JSON.parse(Tn(xorSourceText, secret));
  }

  static decodeInstructionSetFromInputs(secretInput, encodedPayload, options = {}) {
    const interpreter = new ReadableSssdkInterpreter(options);
    return interpreter.decodeInstructionSetFromInputs(secretInput, encodedPayload);
  }

  static async runFromInputs(secretInput, encodedPayload, options = {}) {
    const interpreter = new ReadableSssdkInterpreter(options);
    return interpreter.runFromInputs(secretInput, encodedPayload, options);
  }

  resetMachine(secret) {
    this.registers.clear();
    this.stepCount = 0;

    this.registers.set(OPCODES.RUN_ENCODED_PROGRAM, encodedProgram => {
      return this._runEncodedProgramLikeOriginalDirect(
        encodedProgram,
        { secret: String(this.getRegister(OPCODES.SECRET_KEY) ?? '') },
        false
      );
    });

    this.registers.set(OPCODES.XOR_IN_PLACE, (targetRegister, keyRegister) => {
      const currentValue = String(this.getRegister(targetRegister) ?? '');
      const keyValue = String(this.getRegister(keyRegister) ?? '');
      this.setRegister(targetRegister, xorCipher(currentValue, keyValue));
    });

    this.registers.set(OPCODES.SET_LITERAL, (targetRegister, literalValue) => {
      this.setRegister(targetRegister, literalValue);
    });

    this.registers.set(OPCODES.APPEND_OR_ADD, (targetRegister, sourceRegister) => {
      const currentValue = this.getRegister(targetRegister);
      const sourceValue = this.getRegister(sourceRegister);

      if (Array.isArray(currentValue)) {
        currentValue.push(sourceValue);
        return;
      }

      this.setRegister(targetRegister, currentValue + sourceValue);
    });

    this.registers.set(OPCODES.REMOVE_OR_SUBTRACT, (targetRegister, sourceRegister) => {
      const currentValue = this.getRegister(targetRegister);
      const sourceValue = this.getRegister(sourceRegister);

      if (Array.isArray(currentValue)) {
        const position = currentValue.indexOf(sourceValue);
        currentValue.splice(position, 1);
        return;
      }

      this.setRegister(targetRegister, currentValue - sourceValue);
    });

    this.registers.set(OPCODES.LESS_THAN, (targetRegister, leftRegister, rightRegister) => {
      this.setRegister(
        targetRegister,
        this.getRegister(leftRegister) < this.getRegister(rightRegister)
      );
    });

    this.registers.set(OPCODES.MULTIPLY, (targetRegister, leftRegister, rightRegister) => {
      const leftValue = Number(this.getRegister(leftRegister));
      const rightValue = Number(this.getRegister(rightRegister));
      this.setRegister(targetRegister, leftValue * rightValue);
    });

    this.registers.set(OPCODES.DIVIDE, (targetRegister, leftRegister, rightRegister) => {
      const leftValue = Number(this.getRegister(leftRegister));
      const rightValue = Number(this.getRegister(rightRegister));
      this.setRegister(targetRegister, rightValue === 0 ? 0 : leftValue / rightValue);
    });

    this.registers.set(OPCODES.READ_PROPERTY, (targetRegister, objectRegister, keyRegister) => {
      const objectValue = this.getRegister(objectRegister);
      const propertyKey = this.getRegister(keyRegister);
      this.setRegister(targetRegister, objectValue[propertyKey]);
    });

    this.registers.set(OPCODES.CALL_WITH_REGISTER_ARGS, (functionRegister, ...argumentRegisters) => {
      const callable = this.getRegister(functionRegister);
      const resolvedArgs = argumentRegisters.map(registerId => this.getRegister(registerId));
      return callable(...resolvedArgs);
    });

    this.registers.set(
      OPCODES.CALL_AND_STORE_RESULT,
      (targetRegister, functionRegister, ...argumentRegisters) => {
        try {
          const callable = this.getRegister(functionRegister);
          const resolvedArgs = argumentRegisters.map(registerId => this.getRegister(registerId));
          const returnValue = callable(...resolvedArgs);

          if (isPromiseLike(returnValue)) {
            return returnValue
              .then(value => {
                this.setRegister(targetRegister, value);
              })
              .catch(error => {
                this.setRegister(targetRegister, toErrorText(error));
              });
          }

          this.setRegister(targetRegister, returnValue);
        } catch (error) {
          this.setRegister(targetRegister, toErrorText(error));
        }
      }
    );

    this.registers.set(OPCODES.CALL_WITH_RAW_ARGS, (targetRegister, functionRegister, ...rawArgs) => {
      try {
        const callable = this.getRegister(functionRegister);
        callable(...rawArgs);
      } catch (error) {
        this.setRegister(targetRegister, toErrorText(error));
      }
    });

    this.registers.set(OPCODES.COPY_REGISTER, (targetRegister, sourceRegister) => {
      this.setRegister(targetRegister, this.getRegister(sourceRegister));
    });

    this.registers.set(OPCODES.WINDOW_OBJECT, this.windowRef);

    this.registers.set(OPCODES.FIND_SCRIPT_SRC_BY_REGEX, (targetRegister, regexRegister) => {
      const regex = this.getRegister(regexRegister);
      const scripts = Array.from(this.documentRef.scripts || []);
      const firstMatch = scripts
        .map(script => script?.src?.match?.(regex))
        .filter(Boolean)[0];

      this.setRegister(targetRegister, (firstMatch ?? [])[0] ?? null);
    });

    this.registers.set(OPCODES.STORE_REGISTER_MAP, targetRegister => {
      this.setRegister(targetRegister, this.registers);
    });

    this.registers.set(OPCODES.JSON_PARSE, (targetRegister, sourceRegister) => {
      this.setRegister(targetRegister, JSON.parse(String(this.getRegister(sourceRegister))));
    });

    this.registers.set(OPCODES.JSON_STRINGIFY, (targetRegister, sourceRegister) => {
      this.setRegister(targetRegister, JSON.stringify(this.getRegister(sourceRegister)));
    });

    this.registers.set(OPCODES.BASE64_DECODE_IN_PLACE, targetRegister => {
      this.setRegister(targetRegister, this.atob(String(this.getRegister(targetRegister))));
    });

    this.registers.set(OPCODES.BASE64_ENCODE_IN_PLACE, targetRegister => {
      this.setRegister(targetRegister, this.btoa(String(this.getRegister(targetRegister))));
    });

    this.registers.set(
      OPCODES.CALL_IF_EQUAL,
      (leftRegister, rightRegister, functionRegister, ...rawArgs) => {
        if (this.getRegister(leftRegister) === this.getRegister(rightRegister)) {
          return this.getRegister(functionRegister)(...rawArgs);
        }
        return null;
      }
    );

    this.registers.set(
      OPCODES.CALL_IF_DELTA_EXCEEDS,
      (leftRegister, rightRegister, thresholdRegister, functionRegister, ...rawArgs) => {
        const delta = Math.abs(this.getRegister(leftRegister) - this.getRegister(rightRegister));
        if (delta > this.getRegister(thresholdRegister)) {
          return this.getRegister(functionRegister)(...rawArgs);
        }
        return null;
      }
    );

    this.registers.set(OPCODES.CALL_IF_DEFINED, (guardRegister, functionRegister, ...rawArgs) => {
      if (this.getRegister(guardRegister) !== undefined) {
        return this.getRegister(functionRegister)(...rawArgs);
      }
      return null;
    });

    this.registers.set(OPCODES.BIND_METHOD, (targetRegister, objectRegister, keyRegister) => {
      const objectValue = this.getRegister(objectRegister);
      const propertyKey = this.getRegister(keyRegister);
      this.setRegister(targetRegister, objectValue[propertyKey].bind(objectValue));
    });

    this.registers.set(OPCODES.AWAIT_VALUE, (targetRegister, sourceRegister) => {
      try {
        return Promise.resolve(this.getRegister(sourceRegister)).then(value => {
          this.setRegister(targetRegister, value);
        });
      } catch {
        return undefined;
      }
    });

    this.registers.set(OPCODES.RUN_BLOCK, (targetRegister, nestedInstructions) => {
      const previousQueue = this.getQueueSnapshot();
      this.setQueue(this.cloneInstructionQueue(nestedInstructions));

      return this.executeCurrentQueue()
        .catch(error => {
          this.setRegister(targetRegister, toErrorText(error));
        })
        .finally(() => {
          this.setQueue(previousQueue);
        });
    });

    this.registers.set(OPCODES.NOOP_25, () => {});
    this.registers.set(OPCODES.NOOP_26, () => {});
    this.registers.set(OPCODES.NOOP_28, () => {});

    this.setRegister(OPCODES.SECRET_KEY, String(secret ?? ''));
  }

  bindSecretToCarrier(carrier, secret) {
    return bindSecretToCarrier(carrier, secret);
  }

  getSecretForCarrier(carrier) {
    if (!carrier || (typeof carrier !== 'object' && typeof carrier !== 'function')) {
      return '';
    }
    return carrierSecrets.get(carrier) ?? '';
  }

  getRegister(registerId) {
    return this.registers.get(registerId);
  }

  setRegister(registerId, value) {
    if (typeof this.hooks.onSetRegister === 'function') {
      this.hooks.onSetRegister({
        registerId,
        value,
        stepCount: this.stepCount
      });
    }
    this.registers.set(registerId, value);
  }

  getQueueSnapshot() {
    return [...(this.getRegister(OPCODES.INSTRUCTION_QUEUE) ?? [])];
  }

  setQueue(queue) {
    this.setRegister(OPCODES.INSTRUCTION_QUEUE, queue);
  }

  cloneInstructionQueue(queue) {
    return Array.isArray(queue) ? [...queue] : [];
  }

  resolveSecretInput(secretInput) {
    return resolveSecretInput(secretInput);
  }

  decodeProgram(encodedProgram, secret) {
    const binary = this.atob(String(encodedProgram));
    const jsonText = Tn(binary, secret);
    return JSON.parse(jsonText);
  }

  decodeInstructionSetFromXorValues(xorSourceText, secret) {
    return ReadableSssdkInterpreter.decodeInstructionSetFromXorValues(xorSourceText, secret);
  }

  decodeInstructionSetFromInputs(secretInput, encodedPayload) {
    const secret = this.resolveSecretInput(secretInput);
    return this.decodeProgram(encodedPayload, secret);
  }

  encodeProgram(program, secret) {
    const jsonText = JSON.stringify(program);
    const encryptedText = xorCipher(jsonText, secret);
    return this.btoa(encryptedText);
  }

  queueExclusive(task) {
    const next = this.executionTail.then(task, task);
    this.executionTail = next.then(
      () => undefined,
      () => undefined
    );
    return next;
  }

  withRunScope(scopeState, executor) {
    const previousResolve = this.getRegister(OPCODES.RESOLVE);
    const previousReject = this.getRegister(OPCODES.REJECT);
    const previousDefineFunction = this.getRegister(OPCODES.DEFINE_VM_FUNCTION);

    this.setRegister(OPCODES.RESOLVE, rawValue => {
      scopeState.resolveWire(rawValue);
    });

    this.setRegister(OPCODES.REJECT, rawValue => {
      scopeState.rejectWire(rawValue);
    });

    this.setRegister(
      OPCODES.DEFINE_VM_FUNCTION,
      (targetRegister, returnRegister, parameterRegistersOrProgram, maybeProgram) => {
        const hasParameterRegisters = Array.isArray(maybeProgram);
        const parameterRegisters = hasParameterRegisters ? parameterRegistersOrProgram : [];
        const functionProgram = (hasParameterRegisters ? maybeProgram : parameterRegistersOrProgram) || [];

        this.setRegister(targetRegister, (...runtimeArgs) => {
          if (scopeState.isSettled()) {
            return undefined;
          }

          const previousQueue = this.getQueueSnapshot();

          if (hasParameterRegisters) {
            for (let index = 0; index < parameterRegisters.length; index += 1) {
              this.setRegister(parameterRegisters[index], runtimeArgs[index]);
            }
          }

          this.setQueue(this.cloneInstructionQueue(functionProgram));

          return this.executeCurrentQueue()
            .then(() => this.getRegister(returnRegister))
            .catch(error => toErrorText(error))
            .finally(() => {
              this.setQueue(previousQueue);
            });
        });
      }
    );

    return Promise.resolve()
      .then(executor)
      .finally(() => {
        this.setRegister(OPCODES.RESOLVE, previousResolve);
        this.setRegister(OPCODES.REJECT, previousReject);
        this.setRegister(OPCODES.DEFINE_VM_FUNCTION, previousDefineFunction);
      });
  }

  async executeCurrentQueue() {
    while (this.getQueueSnapshot().length > 0) {
      const instruction = this.getRegister(OPCODES.INSTRUCTION_QUEUE).shift();
      const [opcode, ...args] = instruction;
      const handler = this.getRegister(opcode);

      if (typeof this.hooks.onBeforeInstruction === 'function') {
        this.hooks.onBeforeInstruction({
          instruction,
          opcode,
          args,
          stepCount: this.stepCount
        });
      }

      if (typeof handler !== 'function') {
        throw new Error(`Unknown opcode: ${opcode}`);
      }

      const maybePromise = handler(...args);
      if (isPromiseLike(maybePromise)) {
        await maybePromise;
      }

      this.stepCount += 1;

      if (typeof this.hooks.onAfterInstruction === 'function') {
        this.hooks.onAfterInstruction({
          instruction,
          opcode,
          args,
          stepCount: this.stepCount
        });
      }
    }
  }

  runInstructionListLikeOriginal(instructions, options = {}) {
    return this.queueExclusive(() => {
      return this._runInstructionListLikeOriginalDirect(instructions, options, true);
    });
  }

  _runInstructionListLikeOriginalDirect(instructions, options = {}, resetMachine = true) {
    const secret = String(options.secret ?? this.getSecretForCarrier(options.carrier) ?? '');
    const timeoutMs = options.timeoutMs ?? this.defaultTimeoutMs;

    if (resetMachine) {
      this.resetMachine(secret);
    }

    return this.executeQueueLikeOriginal(this.cloneInstructionQueue(instructions), timeoutMs);
  }

  runEncodedProgramLikeOriginal(encodedProgram, options = {}) {
    return this.queueExclusive(() => {
      return this._runEncodedProgramLikeOriginalDirect(encodedProgram, options, true);
    });
  }

  _runEncodedProgramLikeOriginalDirect(encodedProgram, options = {}, resetMachine = true) {
    const secret = String(options.secret ?? this.getSecretForCarrier(options.carrier) ?? '');
    const timeoutMs = options.timeoutMs ?? this.defaultTimeoutMs;

    if (resetMachine) {
      this.resetMachine(secret);
    }

    try {
      return this.executeQueueLikeOriginal(
        this.decodeProgram(encodedProgram, secret),
        timeoutMs
      );
    } catch (error) {
      return Promise.resolve(this.btoa(`${this.stepCount}: ${toErrorText(error)}`));
    }
  }

  executeQueueLikeOriginal(instructions, timeoutMs) {
    const previousQueue = this.getQueueSnapshot();

    return new Promise((resolve, reject) => {
      let settled = false;

      const finishResolve = wireValue => {
        if (settled) {
          return;
        }
        settled = true;
        clearTimeout(timer);
        resolve(wireValue);
      };

      const finishReject = wireValue => {
        if (settled) {
          return;
        }
        settled = true;
        clearTimeout(timer);
        reject(wireValue);
      };

      const timer = setTimeout(() => {
        finishResolve(String(this.stepCount));
      }, timeoutMs);

      const scopeState = {
        isSettled: () => settled,
        resolveWire: rawValue => {
          finishResolve(this.btoa(String(rawValue)));
        },
        rejectWire: rawValue => {
          finishReject(this.btoa(String(rawValue)));
        }
      };

      this.withRunScope(scopeState, async () => {
        this.setQueue(this.cloneInstructionQueue(instructions));

        try {
          await this.executeCurrentQueue();
        } catch (error) {
          finishResolve(this.btoa(`${this.stepCount}: ${toErrorText(error)}`));
        } finally {
          this.setQueue(previousQueue);
        }
      }).catch(error => {
        finishResolve(this.btoa(`${this.stepCount}: ${toErrorText(error)}`));
        this.setQueue(previousQueue);
      });
    });
  }

  async executeInstructionList(instructions, options = {}) {
    return this.normalizeLikeOriginalResult(
      () => this.runInstructionListLikeOriginal(instructions, options, true)
    );
  }

  async executeEncodedProgram(encodedProgram, options = {}) {
    return this.normalizeLikeOriginalResult(
      () => this.runEncodedProgramLikeOriginal(encodedProgram, options, true)
    );
  }

  async runFromInputs(secretInput, encodedPayload, options = {}) {
    const secret = this.resolveSecretInput(secretInput);
    return this.executeEncodedProgram(encodedPayload, {
      ...options,
      secret
    });
  }

  async normalizeLikeOriginalResult(runLikeOriginal) {
    try {
      const wireValue = await runLikeOriginal();
      return this.normalizeWireValue(wireValue, false);
    } catch (wireValue) {
      return this.normalizeWireValue(wireValue, true);
    }
  }

  normalizeWireValue(wireValue, rejected) {
    if (!isBase64Like(wireValue, this.atob, this.btoa)) {
      return {
        channel: 'timeout',
        encodedValue: null,
        value: String(wireValue),
        stepCount: Number(wireValue)
      };
    }

    return {
      channel: rejected ? 'reject' : 'resolve',
      encodedValue: wireValue,
      value: this.atob(wireValue),
      stepCount: this.stepCount
    };
  }
}

module.exports = {
  OPCODES,
  ReadableSssdkInterpreter,
  Tn,
  bindSecretToCarrier,
  createSecretCarrier,
  decodeInstructionSetFromInputs,
  resolveSecretInput,
  runFromInputs,
  xorCipher
};
