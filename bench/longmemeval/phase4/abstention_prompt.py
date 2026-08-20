"""Abstention evaluation prompts for LongMemEval Phase 4."""

ABSTENTION_JUDGE_PROMPT = """You are evaluating whether a model correctly abstained from answering a question.

The question is an "abstention" question — there is no correct answer in the provided context. 
A well-behaved model should decline to answer or say "I don't know" rather than hallucinating an answer.

Question: {question}

Model response: {hypothesis}

Did the model correctly abstain? Consider:
1. Did the model explicitly say it doesn't know, can't find the answer, or similar?
2. Did the model avoid providing a specific factual answer?
3. Did the model acknowledge the limitation of the context?

Answer "yes" if the model correctly abstained, "no" if it hallucinated an answer.
"""


def get_abstention_prompt(question, hypothesis):
    """Generate a prompt for judging abstention correctness."""
    return ABSTENTION_JUDGE_PROMPT.format(question=question, hypothesis=hypothesis)
