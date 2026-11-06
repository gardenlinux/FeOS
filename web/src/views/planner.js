// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// planner runs the builder's plan on every change (ADR 0032): delay ms
// after the last change, one plan in flight, the answer to an older
// input dropped. change(input) schedules; run(input) is the plan (a
// promise); onResult(answer, input) and onError(error, input) hear the
// answers to the newest input only. Pure: the timers are passed in for
// the tests.
export function planner({ run, onResult, onError = () => {}, onBusy = () => {}, delay = 500,
  setTimer = setTimeout, clearTimer = clearTimeout }) {
  let seq = 0;
  let latest;
  let timer = null;
  let inFlight = false;
  let pending = false;
  let stopped = false;

  const start = () => {
    const my = seq;
    const input = latest;
    inFlight = true;
    onBusy(true);
    Promise.resolve().then(() => run(input)).then(
      (a) => { if (!stopped && my === seq) onResult(a, input); },
      (e) => { if (!stopped && my === seq) onError(e, input); },
    ).finally(() => {
      inFlight = false;
      if (stopped) return;
      if (pending) {
        pending = false;
        start();
      } else if (timer === null) {
        onBusy(false);
      }
    });
  };

  const fire = () => {
    timer = null;
    if (stopped) return;
    if (inFlight) pending = true;
    else start();
  };

  return {
    change(input) {
      latest = input;
      seq++;
      if (timer !== null) clearTimer(timer);
      onBusy(true);
      timer = setTimer(fire, delay);
    },
    // now plans the newest input at once (after an apply's refusal).
    now(input) {
      if (input !== undefined) { latest = input; seq++; }
      if (timer !== null) { clearTimer(timer); timer = null; }
      fire();
    },
    stop() {
      stopped = true;
      if (timer !== null) clearTimer(timer);
    },
  };
}
