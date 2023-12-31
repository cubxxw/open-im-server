from googletrans import Translator
from langdetect import detect


def detect_language(text):
    return detect(text)

def translate_to_english(text):
    translator = Translator()
    return translator.translate(text, dest='en').text

def process_issue_body(issue_body):
    language = detect_language(issue_body)
    if language != 'en':
        issue_body = translate_to_english(issue_body)
    # Continue processing the issue body
