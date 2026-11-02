package vocab

// systemPrompt explains what vocabulary is for. The current vocabulary is
// appended after it.
const systemPrompt = `You propose vocabulary for Stele, a registry that checks software requirements for contradictions. Requirements are later translated into rules that may use only declared vocabulary, so two requirements can be compared only if they use the same symbols for the same things.

# What vocabulary is

- An entity is a thing the requirements talk about: an order, a user, a session, a configuration, a UI screen, a component of the system, a scheduled job, a file. Requirements about UI, structure ("the system MUST provide X", "MUST use Y", "MUST delete Z") and process ("tests MUST be written first") are requirements too: give them vocabulary as well.
- An attribute is a typed property of an entity. Types: bool; int (min and max when bounded); decimal; enum with values (states, kinds, modes); string (names, routes, commands, messages, identifiers, formats); duration; time (a date or timestamp); list<T>, set<T> and map<K, V> of those (list<enum[a, b]> and map<enum[gold, silver], int> name enum values inline); optional<T> for a value that may be absent. A record's fields are attributes with dotted names on its entity: result.code, result.message. Use bool for facts such as whether a component exists, a screen shows something, or a step was done. Use enum for states. Use the type the statement's values have: a route or log message is a string, a timeout is a duration, not a bool that it is configured.
- A relation links one entity to another (an order's customer). Set many to true when it links to several (a shipment's orders), and optional to true when it may link to nothing (an order's coupon, the selected address).
- An operation is one step: an action someone invokes, or an event the system handles (a scheduled sweep, a message from another service). It lists every attribute it may change in writes; everything else keeps its value. Never list in writes what the world decides: time remaining, elapsed or idle minutes, clocks, what users or external services do. The checker treats anything an operation writes as chosen by the system, so writing a countdown lets the system pretend a deadline is never near and hides real contradictions. Its default is allow when it runs unless a rule forbids it (events, system actions) and deny when it runs only where a rule allows it.

# How to propose

- Model facts, not sentences. A fact gets one attribute on the entity it is about, and every requirement about that fact uses it. "Remind the holder a day before a reservation expires" and "MUST NOT send reminders to opted-out users" are both about whether a reservation's reminder is sent: one attribute such as Reservation.reminderSent, not a separate flag per requirement (not Notifier.sendsExpiryReminders, not Mailer.reminderPolicyEnabled). Prefer attributes that describe state over attributes that restate a requirement.
- Requirements about the same thing in different specs must meet on the same symbols; that is the only way their contradictions can be found.
- Propose only what the statements need and the vocabulary lacks. Reuse an existing symbol whenever it means the same thing, even under a different word. Never propose a synonym of an existing symbol: two names for one thing hide contradictions.
- Add attributes or relations to an existing entity through extensions, never by redeclaring the entity.
- Name entities in UpperCamelCase, attributes and operations in lowerCamelCase. Operation names are Entity.verb, for an existing or proposed entity.
- writes must list attributes as Entity.attribute, of existing or proposed entities.
- For each symbol, list the statement_ids that need it.
- For attributes that are not int or decimal (or a collection of them), set min and max to 0; give values only for enum types.
- Give a number the range its meaning has (a ratio or probability 0 to 1, a percentage 0 to 100, a similarity -1 to 1), and no range when it has none: a counter, a size or a duration in units has no maximum, and an invented maximum makes the next increment impossible. A limit a requirement imposes ("at most 5 books on loan", "a timeout of at most 30s") is a rule, not a range: a range cannot conflict with the requirement that changes the limit.
- An output an operation produces (its result, the status it reports, a value it derives or computes, a row it inserts) is written by that operation, not set by the world, even when it reads only world values; the world sets the inputs.
- What a step produces and the system keeps nowhere (what it returns, the messages, notifications, events, logs and metrics it emits, how many attempts it made) is an attribute of the built-in entity Result, added through an extension named Result: deletedCount, emailSent, attempts. No operation writes Result; each step produces its own.
- Settings, environment variables and storage locations the running system reads are attributes of a configuration entity (Config.retentionDays), set by the world.
- set_by is world for a value that time, clocks, users or external services decide, and system otherwise. No operation lists a world attribute in writes; a step that resets one to a fixed value (activity sets the minutes left to 60) says so in a rule on the step, and Stele records the reset.
- If a statement has nothing to model (pure prose, a template placeholder), propose nothing for it.

# Current vocabulary

`

const outputSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["entities", "extensions", "operations"],
  "properties": {
    "entities": {"type": "array", "items": {"$ref": "#/$defs/entity"}},
    "extensions": {"type": "array", "items": {"$ref": "#/$defs/entity"}},
    "operations": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "statement_ids", "default", "writes", "rationale"],
        "properties": {
          "name": {"type": "string"},
          "statement_ids": {"type": "array", "items": {"type": "string"}},
          "default": {"type": "string", "enum": ["allow", "deny"]},
          "writes": {"type": "array", "items": {"type": "string"}},
          "rationale": {"type": "string"}
        }
      }
    }
  },
  "$defs": {
    "entity": {
      "type": "object",
      "additionalProperties": false,
      "required": ["name", "statement_ids", "attributes", "relations"],
      "properties": {
        "name": {"type": "string"},
        "statement_ids": {"type": "array", "items": {"type": "string"}},
        "attributes": {
          "type": "array",
          "items": {
            "type": "object",
            "additionalProperties": false,
            "required": ["name", "type", "values", "min", "max", "set_by"],
            "properties": {
              "name": {"type": "string"},
              "type": {"type": "string"},
              "values": {"type": "array", "items": {"type": "string"}},
              "min": {"type": "integer"},
              "max": {"type": "integer"},
              "set_by": {"type": "string", "enum": ["system", "world"]}
            }
          }
        },
        "relations": {
          "type": "array",
          "items": {
            "type": "object",
            "additionalProperties": false,
            "required": ["name", "to", "many", "optional"],
            "properties": {"name": {"type": "string"}, "to": {"type": "string"}, "many": {"type": "boolean"}, "optional": {"type": "boolean"}}
          }
        }
      }
    }
  }
}`

// factsPrompt asks for the facts behind the requirements, before any
// naming per requirement.
const factsPrompt = `You prepare vocabulary for Stele, a registry that checks software requirements for contradictions. Two requirements can only be compared if they are stated about the same symbol, so the first step is to find the facts the requirements talk about, across all specs, before writing any rule.

A fact is one property of the system or its world that requirements constrain: whether a reservation's reminder is sent, an order's status, how many items a cart holds, whether a payment was captured, whether a session survives a restart. Requirements about UI, structure and process have facts too: whether a screen shows something, whether a component exists, whether a step was done.

# Rules

- One fact per thing, shared by every statement about it, whichever spec it comes from and whatever words it uses. "Remind the holder a day before expiry", "show a reminder banner before a reservation lapses" and "MUST NOT send reminders" are all about Reservation.reminderSent.
- A fact describes state, not a requirement. Not Store.supportsGuestCheckout or Checkout.singlePageModeSupported (a requirement restated as a flag, which nothing can contradict), but the state the requirement constrains: Checkout.accountMode enum {guest, registered}, Session.persistsAcrossRestart.
- Put each fact on the entity it is about. Name it Entity.attribute: entities in UpperCamelCase, attributes in lowerCamelCase. Reuse entities of the vocabulary below.
- Give a type: bool; int (min and max when bounded); decimal; enum with values (states, kinds, modes); string; duration; time; list<T>, set<T>, map<K, V>; optional<T>. Use the type the statement's values have: a route or message is a string, a timeout a duration. For other than int or decimal facts set min and max to 0; give values only for enum types.
- meaning says the fact in a short plain sentence.
- statement_ids lists every statement in this request that constrains or reads the fact.
- Actions and events (cancel, create, a scheduled sweep) are not facts; they are found later. List what they change or depend on as facts.
- set_by says who decides the fact's value: system when the system's own actions set it, world when time, clocks, users or external services do (minutes until a deadline, idle minutes, what a user typed, whether a remote service answered). The system cannot choose world values; no operation will write them.
- A statement with nothing to model (pure prose, a placeholder) gets no fact.

# Vocabulary

`

const consolidatePrompt = `You review facts proposed as vocabulary for Stele, a registry that checks software requirements for contradictions. Requirements can only contradict each other when they are stated about the same fact, so two names for one fact hide contradictions.

`

const factsSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["facts"],
  "properties": {
    "facts": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["name", "meaning", "type", "values", "min", "max", "set_by", "statement_ids"],
        "properties": {
          "name": {"type": "string"},
          "meaning": {"type": "string"},
          "type": {"type": "string"},
          "values": {"type": "array", "items": {"type": "string"}},
          "min": {"type": "integer"},
          "max": {"type": "integer"},
          "set_by": {"type": "string", "enum": ["system", "world"]},
          "statement_ids": {"type": "array", "items": {"type": "string"}}
        }
      }
    }
  }
}`

const mergeSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["merges"],
  "properties": {
    "merges": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["from", "into", "reason"],
        "properties": {"from": {"type": "string"}, "into": {"type": "string"}, "reason": {"type": "string"}}
      }
    }
  }
}`

// SystemPrompt and OutputSchema are the proposer's prompt (without the
// vocabulary, which follows its last heading) and reply schema.
const (
	SystemPrompt = systemPrompt
	OutputSchema = outputSchema
)
