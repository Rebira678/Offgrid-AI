package router

// GateCase is one labelled message for measuring the complexity gate.
//
// These were written BEFORE the gate was tuned, by me, from imagining what a
// maintenance crew might type. They are not real worker data. Hard means "a
// person would expect this to need deeper thinking than the five tools and a
// small model can do, so it should wait in the queue".
type GateCase struct {
	Text string
	Hard bool
}

// Cases are the labelled messages: 33 that should be answered now and 33 that
// should be deferred.
var Cases = []GateCase{
	// ---- should be answered now (local) ----
	{"list open tasks", false},
	{"what's still open?", false},
	{"new task: check the fuel line, site 2, high priority", false},
	{"note on the pump task: seal is cracked, part ordered", false},
	{"T-3 is now in progress", false},
	{"summary", false},
	{"I finished replacing the gasket", false},
	{"show tasks not done", false},
	{"mark the filter job as done", false},
	{"how many tasks are open", false},
	{"remind me to order gaskets", false},
	{"add a task to suggest a new supplier", false},
	{"what is the status of T-3", false},
	{"show me the high priority ones", false},
	{"close the pump ticket", false},
	{"change the fuel line task to high priority", false},
	{"summarize today", false},
	{"summarize this week", false},
	{"what did I finish yesterday", false},
	{"add a note to the generator task: oil changed", false},
	{"the pump at site 4 is leaking again, make a task", false},
	{"I need to plan the generator service", false},
	{"create task replace belt on conveyor 2", false},
	{"what's the weather tomorrow?", false},
	{"tell me a joke", false},
	{"set T-5 to done", false},
	{"anything high priority left?", false},
	{"list tasks for site 3", false},
	{"show everything created today", false},
	{"please add a note on T-2: waiting for parts", false},
	{"is T-4 done", false},
	{"open tasks at site 1", false},
	{"hello", false},

	// ---- should be deferred (hard) ----
	{"summarize this week's incidents and suggest priorities", true},
	{"what should I prioritize this week?", true},
	{"explain why the pump keeps failing", true},
	{"recommend a maintenance schedule for the generators", true},
	{"analyze the leak reports from this month", true},
	{"why does the fuel line keep cracking", true},
	{"what's the best way to cut downtime at site 2", true},
	{"give me a rundown of what went wrong at site 2 and what to do next", true},
	{"which site needs attention first", true},
	{"compare this week's failures with last week", true},
	{"draft a handover note for the night shift", true},
	{"write a report for the manager about this month's incidents", true},
	{"what patterns do you see in the pump failures", true},
	{"how can we reduce repeat failures", true},
	{"should we replace the pump or keep repairing it", true},
	{"what is the root cause of the repeated leaks", true},
	{"advise me on how to schedule the crews tomorrow", true},
	{"forecast how many tasks we will have next month", true},
	{"evaluate whether the new filter supplier is better", true},
	{"suggest an order for tackling the open tasks", true},
	{"any ideas for reducing pump failures", true},
	{"how are we doing compared to last week", true},
	{"help me decide which repairs to do first", true},
	{"explain the difference between the two generator faults", true},
	{"what happened at site 2 this week and what should we do about it", true},
	{"assess the risk of postponing the boiler service", true},
	{"plan next week's work for the team based on the open tasks", true},
	{"is the fuel line problem getting worse", true},
	{"give me advice on handling the overdue tasks", true},
	{"think about what we are missing in our inspection routine", true},
	{"prioritise the open tasks for me", true},
	{"summarise the incidents and recommend fixes", true},
	{"why were so many tasks left open last week", true},
}

// HeldOut are cases written AFTER the gate was tuned and never used to tune it.
// They estimate how the gate does on messages it has not been fitted to. They
// are still written by one person (me), so treat the result as a rough guide,
// not a measurement of real crews.
var HeldOut = []GateCase{
	// answer now
	{"add a task: grease the bearings at site 6", false},
	{"what is open at site 2", false},
	{"update the compressor task to in progress", false},
	{"put a note on T-9 saying the valve is stuck", false},
	{"how many are done", false},
	{"mark T-1 and T-2 as finished", false},
	{"show me what is high priority", false},
	{"new task inspect the roof drain high priority", false},
	{"I replaced the filter on the north pump", false},
	{"what did we close yesterday", false},
	{"summarize yesterday", false},
	{"list everything", false},
	{"thanks", false},
	{"delete everything", false},
	{"need to plan a visit to site 5", false},
	{"add a task to explain the new procedure to Sam", false},
	{"is the boiler task still open", false},
	{"set the roof drain job to low priority", false},
	{"tasks for the night shift", false},
	{"read me the notes on T-4", false},
	// defer
	{"what is the likely cause of the repeated overheating", true},
	{"how should we split the open tasks between two crews", true},
	{"is it worth replacing the old compressor", true},
	{"summarize this month's incidents and highlight anything unusual", true},
	{"which tasks should wait until next week", true},
	{"can you review the week and tell me what to fix first", true},
	{"what are the biggest risks at site 3", true},
	{"give me recommendations for the winter shutdown", true},
	{"write up a short report on the generator outage", true},
	{"are the failures at site 2 getting more frequent", true},
	{"what should the team focus on tomorrow", true},
	{"why did the fuel line task take so long", true},
	{"compare the pump repairs at sites 1 and 4", true},
	{"figure out what the common cause of the leaks is", true},
	{"suggest which jobs to hand to the contractor", true},
	{"what would you do about the recurring valve problem", true},
	{"analyse our response times", true},
	{"after this week's problems, what should we change in the checklist", true},
	{"draft an email to the supplier about the late parts", true},
	{"what is going wrong with the north pump", true},
}
