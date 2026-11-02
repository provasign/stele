# Example: elevator controller firmware

Firmware for an elevator controller, written in C, with safety, drive and service areas, each specified separately. One owner, the lead engineer, approves the registry. No users, no HTTP, no database: actors are devices and people at the machine.

## Contexts

| Context | Holds |
| --- | --- |
| `safety` | Interlocks, operating modes |
| `drive` | `Car` (speed, direction), motor commands |
| `doors` | `Door` (state, obstruction sensor) |
| `service` | Inspection procedures |

The project extends `platform.Actor` with `kind: [controller, operator, technician]` and `platform.Request` with `source: [hallCall, carCall, inspectionPanel]`.

```yaml
context: drive
theories: [bitvector]          # status registers are 16-bit
```

```yaml
entity: Car
context: drive
attributes:
  mode:     { type: enum, values: [normal, inspection, fireService, outOfService] }
  speedMmS: { type: int, range: [0, 2500], unit: mm/s }
  status:   { type: bv16 }      # hardware status register
  landingDoor: { type: ref doors.Door }
  roofDoor:    { type: ref doors.Door }
stateMachine:
  field: mode
  transitions:
    - { from: normal, to: inspection, via: enterInspection }
    - { from: inspection, to: normal, via: exitInspection }
    - { from: [normal, inspection], to: outOfService, via: fault }
anchors:
  - { kind: symbol, ref: car_state_t, file: src/drive/car.h }
  - { kind: register, ref: "STATUS@0x4000_1004" }
```

## Rules

```yaml
id: S1
kind: invariant
when: self.speedMmS > 0
assert: self.landingDoor.state = closed and self.roofDoor.state = closed
final: true                    # a safety obligation: no decision may override it
prose: "The car never moves with any door open."
---
id: S4
kind: precondition
operation: Car.move
when: (self.status & 0x0004) = 0     # bit 2: overspeed governor tripped
```

## The contradiction

A year later, a service spec says: "During inspection, a technician on the car roof can jog the car at up to 0.3 m/s with the roof door open." The translator emits an allow for `Car.move` under `mode = inspection and request.source = inspectionPanel and self.speedMmS <= 300`.

The checker reports:

> In inspection mode, a jog request from the inspection panel at 300 mm/s with the roof door open would violate S1. S1 is `final`: no decision can supersede it.

## Resolution

Because S1 is `final`, no decision record can override it. The owner must amend S1 itself, a change to a safety rule that the safety analysis has to support:

```yaml
id: S1
version: 2
kind: invariant
when: self.speedMmS > 0
assert: self.landingDoor.state = closed
  and (self.roofDoor.state = closed
       or (self.mode = inspection and self.speedMmS <= 300))
final: true
```

The checker re-runs on every rule touching `Car.move`, `Car.mode` and `Door.state`, and passes. Generated C tests (Unity) are seeded with the boundary cases: 300 and 301 mm/s, roof door and landing door.

## Assurance levels

| Rule | Level |
| --- | --- |
| S1, S4, mode state machine | Proved (S4 uses the bit-vector tier) |
| "Doors close within 3 s of a call" | Verified (`measured`, hardware-in-the-loop test) |
| "Speed control loop has less than 5% overshoot" | Verified (`measured`); the control law itself is not proved |
| Thread-level races in the scheduler | `obligation` awaiting bounded-trace checks; unchecked until then |
